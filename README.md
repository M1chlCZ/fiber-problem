# fiber-problem

RFC 7807 problem responses and safe request middleware for Fiber v3.

## Features

- An error handler that writes `application/problem+json`.
- Secure request IDs. The middleware removes a client header and sets a random 128-bit ID.
- Panic recovery with a sanitized log entry.
- Structured access logs with the request ID, method, route, status, and duration.
- `no-store` on the paths that you mark as sensitive.
- A body limit with an optional exemption function.
- Per-route timeouts that cancel the request context.

## Install

```
go get github.com/m1chlcz/fiber-problem
```

Go 1.27 or later is required.

## Usage

```go
app := fiber.New(fiber.Config{ErrorHandler: fiberproblem.ErrorHandler})
app.Use(fiberproblem.Recovery(logger))
app.Use(fiberproblem.RequestID())
app.Use(fiberproblem.AccessLog(logger, []string{"/account", "/admin"}))
app.Use(fiberproblem.BodyLimit(1<<20, func(c fiber.Ctx) bool { return c.Path() == "/upload" }))

app.Get("/slow", fiberproblem.Timeout(5*time.Second, handler))

return fiberproblem.WriteFields(c, fiber.StatusUnprocessableEntity,
	"validation_failed", "Validation failed", "One or more request fields are invalid.",
	map[string]string{"email": "required"})
```

## Problem fields

| Field | Meaning |
| --- | --- |
| `status` | The HTTP status code. |
| `code` | A stable code such as `not_found` or `internal_error`. |
| `title` | A short label. |
| `detail` | A safe detail. Internal errors become a generic text. |
| `field_errors` | A map of field to message. Never null. |
| `request_id` | The request ID from the response. |

## Codes

`unauthorized`, `forbidden`, `conflict`, `validation_failed`, `rate_limited`, `invalid_request`, `not_found`, `method_not_allowed`, `request_timeout`, `payload_too_large`, `unsupported_media_type`, `service_unavailable`, `internal_error`.

## Notes

- The timeout wrapper is cooperative. Fiber response contexts are not concurrency-safe, so pass cancellation through `c.Context()`.
- A timeout applies only to the handlers that you wrap.
- The access log holds no query string, header, or body data.
- Recovery reports a panic as `http_panic`. The access line covers the requests that return.
- `Recovery` returns `ErrPanic`, so the error handler writes a generic 500 problem.

## Development

```
go test -race ./...
```

## License

MIT. See `LICENSE`.
