package http

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

const (
	ctxRequestID = "request_id"
	ctxPrincipal = "principal"
)

// RequestID assigns a correlation id used in logs, error envelopes and traces.
func RequestID() fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := c.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			buf := make([]byte, 8)
			_, _ = rand.Read(buf)
			id = hex.EncodeToString(buf)
		}
		c.Locals(ctxRequestID, id)
		c.Set("X-Request-ID", id)
		return c.Next()
	}
}

// Logger writes one structured line per request.
func Logger(log *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()

		status := c.Response().StatusCode()
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}
		// Health checks at info level would drown the log; they are only
		// interesting when they fail.
		if c.Path() == "/health" && status < 400 {
			level = slog.LevelDebug
		}

		log.Log(c.Context(), level, "http request",
			"method", c.Method(),
			"path", c.Path(),
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", c.Locals(ctxRequestID),
			"ip", c.IP(),
		)
		return err
	}
}

// Recover turns a panic into a 500 rather than terminating the worker.
func Recover(log *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) (err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic recovered",
					"panic", r, "path", c.Path(), "request_id", c.Locals(ctxRequestID))
				err = domain.Unavailable("internal_error", "an unexpected error occurred")
			}
		}()
		return c.Next()
	}
}

// CORS applies a strict origin allowlist. Wildcard plus credentials is never
// permitted: that combination lets any site read authenticated responses.
func CORS(origins []string) fiber.Handler {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	return func(c *fiber.Ctx) error {
		origin := c.Get("Origin")
		if origin != "" && allowed[origin] {
			c.Set("Access-Control-Allow-Origin", origin)
			c.Set("Access-Control-Allow-Credentials", "true")
			c.Set("Vary", "Origin")
			c.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID, Idempotency-Key")
			c.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			c.Set("Access-Control-Max-Age", "600")
		}
		if c.Method() == fiber.MethodOptions {
			return c.SendStatus(fiber.StatusNoContent)
		}
		return c.Next()
	}
}

// SecurityHeaders sets defensive response headers.
func SecurityHeaders() fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("X-Frame-Options", "DENY")
		c.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Set("Cross-Origin-Opener-Policy", "same-origin")
		return c.Next()
	}
}

// Authenticate validates the bearer token and attaches the principal.
func Authenticate(issuer port.TokenIssuer) fiber.Handler {
	return func(c *fiber.Ctx) error {
		token := bearerToken(c)
		if token == "" {
			return domain.Unauthorized("authentication required")
		}
		principal, err := issuer.Parse(token)
		if err != nil {
			return err
		}
		c.Locals(ctxPrincipal, principal)
		return c.Next()
	}
}

// bearerToken extracts a token from the Authorization header, falling back to
// the query string for websocket upgrades (browsers cannot set headers there).
func bearerToken(c *fiber.Ctx) string {
	header := c.Get("Authorization")
	if header != "" {
		if parts := strings.SplitN(header, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	return strings.TrimSpace(c.Query("token"))
}

// principalOf returns the authenticated identity attached by Authenticate.
func principalOf(c *fiber.Ctx) (domain.Principal, error) {
	p, ok := c.Locals(ctxPrincipal).(domain.Principal)
	if !ok || p.UserID.IsZero() {
		return domain.Principal{}, domain.Unauthorized("authentication required")
	}
	return p, nil
}

// RequireRole enforces a minimum account role.
func RequireRole(min domain.Role) fiber.Handler {
	return func(c *fiber.Ctx) error {
		p, err := principalOf(c)
		if err != nil {
			return err
		}
		if !p.Role.AtLeast(min) {
			return domain.Forbidden("your role does not permit this action")
		}
		return c.Next()
	}
}

// RateLimit is a fixed-window per-key limiter.
//
// Authentication endpoints without a limiter are a credential-stuffing
// invitation. This implementation is in-process and therefore per-replica;
// v0.2 moves the counter to Redis so the limit is global. A per-replica limit
// is still far better than none.
type RateLimit struct {
	mu       sync.Mutex
	counters map[string]*window
	limit    int
	period   time.Duration
}

type window struct {
	count int
	reset time.Time
}

// NewRateLimit constructs a limiter allowing limit requests per period.
func NewRateLimit(limit int, period time.Duration) *RateLimit {
	rl := &RateLimit{counters: map[string]*window{}, limit: limit, period: period}
	go rl.reap()
	return rl
}

// reap discards expired windows so the map cannot grow without bound under a
// distributed source of traffic.
func (rl *RateLimit) reap() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		rl.mu.Lock()
		for key, w := range rl.counters {
			if now.After(w.reset) {
				delete(rl.counters, key)
			}
		}
		rl.mu.Unlock()
	}
}

// Handler returns the Fiber middleware.
func (rl *RateLimit) Handler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		key := c.IP() + ":" + c.Path()
		now := time.Now()

		rl.mu.Lock()
		w, ok := rl.counters[key]
		if !ok || now.After(w.reset) {
			w = &window{reset: now.Add(rl.period)}
			rl.counters[key] = w
		}
		w.count++
		count, reset := w.count, w.reset
		rl.mu.Unlock()

		if count > rl.limit {
			retryAfter := int(time.Until(reset).Seconds()) + 1
			c.Set("Retry-After", itoa(retryAfter))
			return &domain.Error{
				Code:    "rate_limited",
				Message: "too many requests; please slow down",
				Kind:    domain.ErrUnavailable,
			}
		}
		return c.Next()
	}
}

func itoa(n int) string {
	if n <= 0 {
		return "1"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ErrorHandler is the single translation point from domain errors to HTTP.
func ErrorHandler(log *slog.Logger) fiber.ErrorHandler {
	return func(c *fiber.Ctx, err error) error {
		requestID, _ := c.Locals(ctxRequestID).(string)

		var de *domain.Error
		if errors.As(err, &de) {
			body := fiber.Map{"code": de.Code, "message": de.Message, "request_id": requestID}
			if len(de.Fields) > 0 {
				body["fields"] = de.Fields
			}
			status := statusFor(de.Kind)
			if status >= 500 {
				log.Error("request failed", "code", de.Code, "error", err, "request_id", requestID)
			}
			return c.Status(status).JSON(fiber.Map{"error": body})
		}

		var fe *fiber.Error
		if errors.As(err, &fe) {
			return c.Status(fe.Code).JSON(fiber.Map{"error": fiber.Map{
				"code": "http_error", "message": fe.Message, "request_id": requestID}})
		}

		// Anything unrecognised is a bug: log it in full, tell the client
		// nothing that would help an attacker.
		log.Error("unhandled error", "error", err, "path", c.Path(), "request_id", requestID)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fiber.Map{
			"code": "internal_error", "message": "an unexpected error occurred", "request_id": requestID}})
	}
}

func statusFor(kind error) int {
	switch {
	case errors.Is(kind, domain.ErrNotFound):
		return fiber.StatusNotFound
	case errors.Is(kind, domain.ErrConflict):
		return fiber.StatusConflict
	case errors.Is(kind, domain.ErrValidation):
		return fiber.StatusUnprocessableEntity
	case errors.Is(kind, domain.ErrUnauthorized):
		return fiber.StatusUnauthorized
	case errors.Is(kind, domain.ErrForbidden):
		return fiber.StatusForbidden
	case errors.Is(kind, domain.ErrUnavailable):
		return fiber.StatusServiceUnavailable
	case errors.Is(kind, domain.ErrCanceled):
		return fiber.StatusRequestTimeout
	}
	return fiber.StatusInternalServerError
}
