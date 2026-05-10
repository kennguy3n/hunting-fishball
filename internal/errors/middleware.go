package errors

import (
	"errors"

	"github.com/gin-gonic/gin"
)

// Response is the JSON body shape emitted by the middleware.
//
//	{
//	  "error": {
//	    "code":    "ERR_TENANT_NOT_FOUND",
//	    "message": "tenant not found",
//	    "retry":   "",
//	    "details": { ... }
//	  }
//	}
type Response struct {
	Error Body `json:"error"`
}

// Body is the inner payload of Response.
type Body struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Retry   string         `json:"retry,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// Middleware returns a Gin handler that converts any error
// attached via c.Error(...) (or returned by the handler chain
// via panic) into the structured JSON envelope.
//
// Handlers signal a typed error by calling
//
//	c.Error(errcat.Wrap(errcat.CodeNotFound, err))
//	return
//
// The middleware runs c.Next(), inspects c.Errors, and emits the
// envelope. Plain (untyped) errors are mapped to CodeUnknown.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if len(c.Errors) == 0 {
			return
		}
		// Use the last attached error as the canonical one — the
		// handler is expected to attach a single typed error before
		// returning, but if multiple are present we surface the
		// most recent one (consistent with stdlib log behaviour).
		last := c.Errors.Last()
		var typed *Error
		if !errors.As(last.Err, &typed) {
			typed = Wrap(CodeUnknown, last.Err)
		}
		entry := Resolve(typed)
		body := Body{
			Code:    typed.Code,
			Message: messageOrFallback(typed.Message, entry.Message),
			Retry:   entry.Retry,
			Details: typed.Details,
		}
		c.AbortWithStatusJSON(entry.HTTPStatus, Response{Error: body})
	}
}

func messageOrFallback(custom, fallback string) string {
	if custom != "" {
		return custom
	}
	return fallback
}
