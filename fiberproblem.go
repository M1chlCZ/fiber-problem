// Package fiberproblem adds RFC 7807 problem responses and safe request
// middleware to a Fiber v3 application.
package fiberproblem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
)

// DefaultTimeout bounds a handler when Timeout receives a value below one.
const DefaultTimeout = 30 * time.Second

// ErrPanic is returned after Recovery catches a panic.
var ErrPanic = errors.New("recovered panic")

// Problem is an RFC 7807 response body with a stable code and a request ID.
type Problem struct {
	Status      int               `json:"status"`
	Code        string            `json:"code"`
	Title       string            `json:"title"`
	Detail      string            `json:"detail"`
	FieldErrors map[string]string `json:"field_errors"`
	RequestID   string            `json:"request_id"`
}

type requestIDKey struct{}

// RequestID returns a middleware that replaces a client request ID with a
// random 128-bit value and stores it in the request context.
func RequestID() fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Request().Header.Del(fiber.HeaderXRequestID)
		requestID := newRequestID()
		c.Set(fiber.HeaderXRequestID, requestID)
		c.SetContext(contextWithRequestID(c.Context(), requestID))
		return c.Next()
	}
}

// RequestIDFromContext returns the request ID stored by RequestID.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}

// ResponseRequestID returns the request ID that the response carries.
func ResponseRequestID(c fiber.Ctx) string {
	return strings.Clone(c.GetRespHeader(fiber.HeaderXRequestID))
}

// Recovery returns a middleware that logs a panic and returns ErrPanic.
func Recovery(logger *slog.Logger) fiber.Handler {
	observed := loggerOrDiscard(logger)
	return fiberrecover.New(fiberrecover.Config{PanicHandler: func(c fiber.Ctx, _ any) error {
		logPanic(observed, c)
		return ErrPanic
	}})
}

// AccessLog returns a middleware that writes one structured log line for each
// request that completes. Recovery reports a panic instead. A request under a
// sensitive prefix receives no-store.
func AccessLog(logger *slog.Logger, sensitivePrefixes []string) fiber.Handler {
	observed := loggerOrDiscard(logger)
	return func(c fiber.Ctx) error {
		start := time.Now()
		if isSensitive(c.Path(), sensitivePrefixes) {
			c.Set(fiber.HeaderCacheControl, "no-store")
		}
		err := c.Next()
		status := c.Response().StatusCode()
		if err != nil {
			status = FromError(err).Status
		}
		observed.Log(c.Context(), slog.LevelInfo, "http_request",
			"request_id", ResponseRequestID(c),
			"method", strings.Clone(c.Method()),
			"route", strings.Clone(c.Route().Path),
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return err
	}
}

// BodyLimit returns a middleware that rejects a body above limit bytes. A
// non-nil allow function exempts a request.
func BodyLimit(limit int, allow func(fiber.Ctx) bool) fiber.Handler {
	return func(c fiber.Ctx) error {
		if limit <= 0 || (allow != nil && allow(c)) {
			return c.Next()
		}
		if len(c.Body()) > limit || c.Response().StatusCode() == fiber.StatusRequestEntityTooLarge {
			return fiber.ErrRequestEntityTooLarge
		}
		return c.Next()
	}
}

// Timeout returns a handler with its own context deadline. The wrapper returns
// 408 when the deadline passes.
func Timeout(timeout time.Duration, handler fiber.Handler) fiber.Handler {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return func(c fiber.Ctx) error {
		parent := c.Context()
		requestContext, cancel := context.WithTimeout(parent, timeout)
		c.SetContext(requestContext)
		defer func() {
			c.SetContext(parent)
			cancel()
		}()
		err := handler(c)
		if errors.Is(requestContext.Err(), context.DeadlineExceeded) ||
			errors.Is(err, context.DeadlineExceeded) {
			return fiber.ErrRequestTimeout
		}
		return err
	}
}

// ErrorHandler writes every error as an application/problem+json response.
func ErrorHandler(c fiber.Ctx, err error) error {
	item := FromError(err)
	return WriteFields(c, item.Status, item.Code, item.Title, item.Detail, item.FieldErrors)
}

// Write writes an application/problem+json response.
func Write(c fiber.Ctx, status int, code, title, detail string) error {
	return WriteFields(c, status, code, title, detail, map[string]string{})
}

// WriteFields writes an application/problem+json response with field errors.
func WriteFields(c fiber.Ctx, status int, code, title, detail string, fieldErrors map[string]string) error {
	if fieldErrors == nil {
		fieldErrors = map[string]string{}
	}
	return c.Status(status).JSON(Problem{
		Status:      status,
		Code:        code,
		Title:       title,
		Detail:      detail,
		FieldErrors: fieldErrors,
		RequestID:   ResponseRequestID(c),
	}, "application/problem+json")
}

// FromError maps an error to a safe public problem. Unknown errors become a
// generic internal error.
func FromError(err error) Problem {
	status := fiber.StatusInternalServerError
	if fiberError, ok := errors.AsType[*fiber.Error](err); ok {
		status = fiberError.Code
	}
	switch status {
	case fiber.StatusUnauthorized:
		return Problem{
			Status: status,
			Code:   "unauthorized",
			Title:  "Unauthorized",
			Detail: "Authentication is required or the supplied credentials are invalid.",
		}
	case fiber.StatusForbidden:
		return Problem{Status: status, Code: "forbidden", Title: "Forbidden", Detail: "The request is not permitted."}
	case fiber.StatusConflict:
		return Problem{
			Status: status,
			Code:   "conflict",
			Title:  "Conflict",
			Detail: "The request conflicts with the current resource state.",
		}
	case fiber.StatusUnprocessableEntity:
		return Problem{
			Status: status,
			Code:   "validation_failed",
			Title:  "Validation failed",
			Detail: "One or more request fields are invalid.",
		}
	case fiber.StatusTooManyRequests:
		return Problem{
			Status: status,
			Code:   "rate_limited",
			Title:  "Too many requests",
			Detail: "Too many requests were made. Try again later.",
		}
	case fiber.StatusBadRequest:
		return Problem{
			Status: status,
			Code:   "invalid_request",
			Title:  "Invalid request",
			Detail: "The request could not be processed.",
		}
	case fiber.StatusNotFound:
		return Problem{
			Status: status,
			Code:   "not_found",
			Title:  "Not found",
			Detail: "The requested resource was not found.",
		}
	case fiber.StatusMethodNotAllowed:
		return Problem{
			Status: status,
			Code:   "method_not_allowed",
			Title:  "Method not allowed",
			Detail: "The request method is not allowed for this resource.",
		}
	case fiber.StatusRequestTimeout:
		return Problem{
			Status: status,
			Code:   "request_timeout",
			Title:  "Request timeout",
			Detail: "The request exceeded its time limit.",
		}
	case fiber.StatusRequestEntityTooLarge:
		return Problem{
			Status: status,
			Code:   "payload_too_large",
			Title:  "Payload too large",
			Detail: "The request body exceeds the allowed size.",
		}
	case fiber.StatusUnsupportedMediaType:
		return Problem{
			Status: status,
			Code:   "unsupported_media_type",
			Title:  "Unsupported media type",
			Detail: "The request media type is not supported.",
		}
	case fiber.StatusServiceUnavailable:
		return Problem{
			Status: status,
			Code:   "service_unavailable",
			Title:  "Service unavailable",
			Detail: "The service is not ready to accept this request.",
		}
	default:
		return Problem{
			Status: fiber.StatusInternalServerError,
			Code:   "internal_error",
			Title:  "Internal server error",
			Detail: "An unexpected error occurred.",
		}
	}
}

// NewJSONLogger returns a JSON logger that writes UTC timestamps.
func NewJSONLogger(output io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey && attr.Value.Kind() == slog.KindTime {
				attr.Value = slog.TimeValue(attr.Value.Time().UTC())
			}
			return attr
		},
	}))
}

func loggerOrDiscard(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return logger
}

func logPanic(logger *slog.Logger, c fiber.Ctx) {
	logger.ErrorContext(c.Context(), "http_panic",
		"request_id", ResponseRequestID(c),
		"stack", string(debug.Stack()),
	)
}

func contextWithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(value[:])
}

func isSensitive(path string, prefixes []string) bool {
	path = strings.ToLower(path)
	for _, prefix := range prefixes {
		prefix = strings.ToLower(prefix)
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
