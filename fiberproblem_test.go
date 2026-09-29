package fiberproblem_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	fiberproblem "github.com/m1chlcz/fiber-problem"
)

const testTimeout = 2 * time.Second

func do(t *testing.T, app *fiber.App, request *http.Request) *http.Response {
	t.Helper()
	response, err := app.Test(request, fiber.TestConfig{Timeout: testTimeout, FailOnTimeout: true})
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return response
}

func decodeProblem(t *testing.T, response *http.Response) fiberproblem.Problem {
	t.Helper()
	defer func() { _ = response.Body.Close() }()
	var item fiberproblem.Problem
	if err := json.NewDecoder(response.Body).Decode(&item); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	return item
}

func TestRequestIDReplacesClientHeader(t *testing.T) {
	t.Parallel()
	captured := make(chan string, 1)
	app := fiber.New(fiber.Config{ErrorHandler: fiberproblem.ErrorHandler})
	app.Use(fiberproblem.RequestID())
	app.Get("/id", func(c fiber.Ctx) error {
		captured <- fiberproblem.RequestIDFromContext(c.Context())
		return c.SendStatus(fiber.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/id", nil)
	request.Header.Set(fiber.HeaderXRequestID, "attacker-controlled")
	response := do(t, app, request)
	defer func() { _ = response.Body.Close() }()
	responseID := response.Header.Get(fiber.HeaderXRequestID)
	if len(responseID) != 32 || responseID == "attacker-controlled" {
		t.Fatalf("unsafe response request ID %q", responseID)
	}
	if contextID := <-captured; contextID != responseID {
		t.Fatalf("context request ID = %q, response = %q", contextID, responseID)
	}
}

type problemCase struct {
	name, method, path string
	status             int
	code               string
}

func TestProblemContractForCommonErrors(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := fiberproblem.NewJSONLogger(&logs)
	app := fiber.New(fiber.Config{ErrorHandler: fiberproblem.ErrorHandler})
	app.Use(fiberproblem.RequestID())
	app.Use(fiberproblem.Recovery(logger))
	app.Use(fiberproblem.AccessLog(logger, nil))
	app.Get("/ok", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	app.Get("/panic", func(fiber.Ctx) error { panic("sensitive panic payload") })

	for _, test := range []problemCase{
		{name: "not found", method: http.MethodGet, path: "/missing", status: http.StatusNotFound, code: "not_found"},
		{
			name: "method not allowed", method: http.MethodPost, path: "/ok",
			status: http.StatusMethodNotAllowed, code: "method_not_allowed",
		},
		{
			name: "panic", method: http.MethodGet, path: "/panic",
			status: http.StatusInternalServerError, code: "internal_error",
		},
	} {
		assertProblemContract(t, app, test)
	}
	if strings.Contains(logs.String(), "sensitive panic payload") {
		t.Fatalf("logs leaked the panic payload: %s", logs.String())
	}
}

func assertProblemContract(t *testing.T, app *fiber.App, test problemCase) {
	t.Helper()
	request := httptest.NewRequest(test.method, test.path, nil)
	request.Header.Set(fiber.HeaderXRequestID, "attacker-controlled")
	response := do(t, app, request)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != test.status {
		t.Fatalf("%s status = %d, want %d", test.name, response.StatusCode, test.status)
	}
	if contentType := response.Header.Get(fiber.HeaderContentType); contentType != "application/problem+json" {
		t.Fatalf("%s Content-Type = %q", test.name, contentType)
	}
	requestID := response.Header.Get(fiber.HeaderXRequestID)
	item := decodeProblem(t, response)
	if item.Status != test.status || item.Code != test.code || item.Title == "" || item.Detail == "" {
		t.Fatalf("%s problem = %#v", test.name, item)
	}
	if item.RequestID != requestID || requestID == "attacker-controlled" {
		t.Fatalf("%s problem request_id = %q, header = %q", test.name, item.RequestID, requestID)
	}
	if item.FieldErrors == nil {
		t.Fatalf("%s field_errors must be an object", test.name)
	}
	if strings.Contains(item.Detail, "sensitive") || strings.Contains(item.Detail, "panic") {
		t.Fatalf("%s problem leaked internal error: %#v", test.name, item)
	}
}

func TestFromErrorMapsFiberStatuses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{err: fiber.ErrUnauthorized, status: fiber.StatusUnauthorized, code: "unauthorized"},
		{err: fiber.ErrForbidden, status: fiber.StatusForbidden, code: "forbidden"},
		{err: fiber.ErrConflict, status: fiber.StatusConflict, code: "conflict"},
		{err: fiber.ErrUnprocessableEntity, status: fiber.StatusUnprocessableEntity, code: "validation_failed"},
		{err: fiber.ErrTooManyRequests, status: fiber.StatusTooManyRequests, code: "rate_limited"},
		{err: fiber.ErrBadRequest, status: fiber.StatusBadRequest, code: "invalid_request"},
		{err: fiber.ErrNotFound, status: fiber.StatusNotFound, code: "not_found"},
		{err: fiber.ErrMethodNotAllowed, status: fiber.StatusMethodNotAllowed, code: "method_not_allowed"},
		{err: fiber.ErrRequestTimeout, status: fiber.StatusRequestTimeout, code: "request_timeout"},
		{err: fiber.ErrRequestEntityTooLarge, status: fiber.StatusRequestEntityTooLarge, code: "payload_too_large"},
		{err: fiber.ErrUnsupportedMediaType, status: fiber.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{err: fiber.ErrServiceUnavailable, status: fiber.StatusServiceUnavailable, code: "service_unavailable"},
		{err: errors.New("boom"), status: fiber.StatusInternalServerError, code: "internal_error"},
	} {
		item := fiberproblem.FromError(test.err)
		if item.Status != test.status || item.Code != test.code || item.Title == "" || item.Detail == "" {
			t.Fatalf("FromError(%v) = %#v", test.err, item)
		}
	}
}

func TestSensitiveResponsesAreNotCached(t *testing.T) {
	t.Parallel()
	app := fiber.New(fiber.Config{ErrorHandler: fiberproblem.ErrorHandler})
	app.Use(fiberproblem.AccessLog(nil, []string{"/account", "/admin"}))
	for _, path := range []string{"/account/missing", "/ADMIN/MISSING"} {
		response := do(t, app, httptest.NewRequest(http.MethodGet, path, nil))
		_ = response.Body.Close()
		if got := response.Header.Get(fiber.HeaderCacheControl); got != "no-store" {
			t.Fatalf("%s Cache-Control = %q", path, got)
		}
	}
	response := do(t, app, httptest.NewRequest(http.MethodGet, "/public", nil))
	_ = response.Body.Close()
	if got := response.Header.Get(fiber.HeaderCacheControl); got == "no-store" {
		t.Fatalf("public path Cache-Control = %q", got)
	}
}

func TestTimeoutBoundsAHandler(t *testing.T) {
	t.Parallel()
	observed := make(chan error, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	app := fiber.New(fiber.Config{ErrorHandler: fiberproblem.ErrorHandler})
	app.Use(fiberproblem.RequestID())
	app.Get("/slow", fiberproblem.Timeout(20*time.Millisecond, func(c fiber.Ctx) error {
		select {
		case <-c.Context().Done():
			observed <- c.Context().Err()
			return c.Context().Err()
		case <-release:
			return nil
		}
	}))
	request := httptest.NewRequest(http.MethodGet, "/slow", nil)
	request.Header.Set(fiber.HeaderXRequestID, "attacker-controlled")
	response := do(t, app, request)
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusRequestTimeout)
	}
	select {
	case err := <-observed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("downstream context error = %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("downstream work did not observe cancellation")
	}
	item := decodeProblem(t, response)
	if item.Code != "request_timeout" {
		t.Fatalf("problem = %#v", item)
	}
}

func TestTimeoutSerializesFiberContextAccess(t *testing.T) {
	t.Parallel()
	app := fiber.New(fiber.Config{ErrorHandler: fiberproblem.ErrorHandler})
	app.Get("/timeout", fiberproblem.Timeout(20*time.Millisecond, func(c fiber.Ctx) error {
		c.Set("X-Before-Timeout", "set")
		<-c.Context().Done()
		c.Set("X-After-Timeout", "set")
		return c.Context().Err()
	}))
	response := do(t, app, httptest.NewRequest(http.MethodGet, "/timeout", nil))
	item := decodeProblem(t, response)
	if response.StatusCode != http.StatusRequestTimeout || item.Code != "request_timeout" {
		t.Fatalf("status = %d, problem = %#v", response.StatusCode, item)
	}
}

func TestTimeoutContextEndsAfterResponse(t *testing.T) {
	t.Parallel()
	captured := make(chan context.Context, 1)
	app := fiber.New(fiber.Config{ErrorHandler: fiberproblem.ErrorHandler})
	app.Get("/context", fiberproblem.Timeout(200*time.Millisecond, func(c fiber.Ctx) error {
		captured <- c.Context()
		return c.SendStatus(fiber.StatusNoContent)
	}))
	response := do(t, app, httptest.NewRequest(http.MethodGet, "/context", nil))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}
	requestContext := <-captured
	select {
	case <-requestContext.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("request context escaped its request lifetime")
	}
}

func TestTimeoutAppliesPerHandler(t *testing.T) {
	t.Parallel()
	app := fiber.New(fiber.Config{ErrorHandler: fiberproblem.ErrorHandler})
	app.Get("/bounded", fiberproblem.Timeout(10*time.Millisecond, func(c fiber.Ctx) error {
		time.Sleep(25 * time.Millisecond)
		return c.SendStatus(fiber.StatusNoContent)
	}))
	app.Get("/unbounded", func(c fiber.Ctx) error {
		time.Sleep(25 * time.Millisecond)
		return c.SendStatus(fiber.StatusNoContent)
	})
	bounded := do(t, app, httptest.NewRequest(http.MethodGet, "/bounded", nil))
	_ = bounded.Body.Close()
	if bounded.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("bounded status = %d", bounded.StatusCode)
	}
	unbounded := do(t, app, httptest.NewRequest(http.MethodGet, "/unbounded", nil))
	_ = unbounded.Body.Close()
	if unbounded.StatusCode != http.StatusNoContent {
		t.Fatalf("unbounded status = %d", unbounded.StatusCode)
	}
}

func TestAccessLogIsStructuredAndRedacted(t *testing.T) {
	t.Parallel()
	const sentinel = "sensitive-sentinel"
	var logs bytes.Buffer
	app := fiber.New(fiber.Config{ErrorHandler: fiberproblem.ErrorHandler})
	app.Use(fiberproblem.RequestID())
	app.Use(fiberproblem.AccessLog(fiberproblem.NewJSONLogger(&logs), nil))
	app.Get("/error", func(fiber.Ctx) error { return errors.New(sentinel) })
	request := httptest.NewRequest(http.MethodGet, "/error?token="+sentinel, strings.NewReader(sentinel))
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+sentinel)
	request.Header.Set(fiber.HeaderCookie, "session="+sentinel)
	response := do(t, app, request)
	_ = response.Body.Close()
	if strings.Contains(logs.String(), sentinel) {
		t.Fatalf("access log leaked request data: %s", logs.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("access log is not one JSON object: %v: %s", err, logs.String())
	}
	for _, field := range []string{"request_id", "method", "route", "status", "duration_ms"} {
		if _, ok := entry[field]; !ok {
			t.Fatalf("access log missing %q: %#v", field, entry)
		}
	}
	if entry["route"] != "/error" {
		t.Fatalf("logged route = %#v", entry["route"])
	}
}

func TestBodyLimit(t *testing.T) {
	t.Parallel()
	app := fiber.New(fiber.Config{ErrorHandler: fiberproblem.ErrorHandler, BodyLimit: 4 << 20})
	app.Use(fiberproblem.RequestID())
	app.Use(fiberproblem.BodyLimit(128, func(c fiber.Ctx) bool { return c.Path() == "/upload" }))
	app.Post("/json", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	app.Post("/upload", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusCreated) })

	large := bytes.Repeat([]byte("a"), 200)
	plain := httptest.NewRequest(http.MethodPost, "/json", bytes.NewReader(large))
	plain.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	assertStatus(t, app, plain, http.StatusRequestEntityTooLarge, "payload_too_large")

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(large); err != nil {
		t.Fatalf("gzip body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close gzip body: %v", err)
	}
	gzipped := httptest.NewRequest(http.MethodPost, "/json", &compressed)
	gzipped.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	gzipped.Header.Set(fiber.HeaderContentEncoding, "gzip")
	assertStatus(t, app, gzipped, http.StatusRequestEntityTooLarge, "payload_too_large")

	allowed := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(large))
	response := do(t, app, allowed)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("allowed upload status = %d", response.StatusCode)
	}

	small := httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(`{"a":1}`))
	small.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	smallResponse := do(t, app, small)
	_ = smallResponse.Body.Close()
	if smallResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("small body status = %d", smallResponse.StatusCode)
	}
}

func TestJSONLoggerWritesUTC(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	logger := fiberproblem.NewJSONLogger(&output)
	logger.Info("hello", "key", "value")
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if entry["msg"] != "hello" {
		t.Fatalf("log entry = %#v", entry)
	}
	timestamp, ok := entry["time"].(string)
	if !ok || !strings.HasSuffix(timestamp, "Z") {
		t.Fatalf("log time = %#v", entry["time"])
	}
}

func assertStatus(t *testing.T, app *fiber.App, request *http.Request, status int, code string) {
	t.Helper()
	response := do(t, app, request)
	if response.StatusCode != status {
		t.Fatalf("status = %d, want %d", response.StatusCode, status)
	}
	if contentType := response.Header.Get(fiber.HeaderContentType); contentType != "application/problem+json" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	item := decodeProblem(t, response)
	if item.Code != code || item.Status != status {
		t.Fatalf("problem = %#v", item)
	}
}
