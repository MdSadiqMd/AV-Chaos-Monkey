package middleware

import (
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		defer func() {
			log.Printf("[HTTP] %s %s %d %v",
				r.Method,
				r.URL.Path,
				ww.Status(),
				time.Since(start))
		}()

		next.ServeHTTP(ww, r)
	})
}
