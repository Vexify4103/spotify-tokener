package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)


type cachedTokenEntry struct {
	Token  []byte
	Expiry time.Time
}

var (
	tokenCache = make(map[string]cachedTokenEntry)
	cacheMutex sync.Mutex
)

type spotifyTokenResponse struct {
	AccessTokenExpirationMillis int64 `json:"accessTokenExpirationTimestampMs,string"`
}

func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var cookies []*network.CookieParam
	for _, cookie := range r.Cookies() {
		cookies = append(cookies, &network.CookieParam{
			Name:  cookie.Name,
			Value: cookie.Value,
			URL:   spotifyURL,
		})
	}

	key := cookiesKey(cookies)

	// --- Cache check ---
	cacheMutex.Lock()
	entry, exists := tokenCache[key]
	if exists && time.Now().Before(entry.Expiry) {
		slog.InfoContext(ctx, "Returning cached Spotify token for key", slog.String("key", key))
		cacheMutex.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(entry.Token)
		return
	}
	cacheMutex.Unlock()
	// -------------------

	body, err := s.getAccessTokenPayload(ctx, cookies)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			slog.ErrorContext(ctx, "Failed to get access token payload", slog.Any("err", err))
		}
		http.Error(w, "Failed to get access token payload", http.StatusInternalServerError)
		return
	}

	expiry := parseExpiry(body)
	slog.InfoContext(ctx, "Parsed Spotify token expiry", slog.Time("expiry", expiry))

	// --- Cache store ---
	cacheMutex.Lock()
	tokenCache[key] = cachedTokenEntry{
		Token:  body,
		Expiry: expiry,
	}
	cacheMutex.Unlock()
	// -------------------

	w.Header().Set("Content-Type", "application/json")
	if _, err = w.Write(body); err != nil {
		slog.ErrorContext(ctx, "Failed to write response", slog.Any("err", err))
	}
}

func (s *server) getAccessTokenPayload(rCtx context.Context, cookies []*network.CookieParam) ([]byte, error) {
	slog.DebugContext(rCtx, "Getting access token payload", slog.Int("cookieCount", len(cookies)))
	ctx, cancel := chromedp.NewContext(s.ctx)
	defer cancel()

	go func() {
		select {
		case <-rCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	requestIDChan := make(chan network.RequestID, 1)
	defer close(requestIDChan)

	chromedp.ListenTarget(ctx, func(ev any) {
		switch ev := ev.(type) {
		case *network.EventResponseReceived:
			if !strings.HasPrefix(ev.Response.URL, spotifyTokenURL) {
				return
			}
			requestIDChan <- ev.RequestID
		}
	})

	if err := chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			if len(cookies) == 0 {
				return nil
			}

			if err := network.SetCookies(cookies).Do(ctx); err != nil {
				return fmt.Errorf("failed to set cookies: %w", err)
			}

			return nil
		}),
		chromedp.Navigate(spotifyURL),
	); err != nil {
		return nil, err
	}

	var requestID network.RequestID
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case requestID = <-requestIDChan:
	}

	var body []byte
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		body, err = network.GetResponseBody(requestID).Do(ctx)
		return err
	})); err != nil {
		return nil, err
	}

	return body, nil
}

func (s *server) startTokenRefresher() {
	go func() {
		for {
			cacheMutex.Lock()
			entry, exists := tokenCache["anonymous"]
			cacheMutex.Unlock()

			if !exists {
				time.Sleep(10 * time.Second)
				continue
			}

			sleepDuration := time.Until(entry.Expiry.Add(-1 * time.Minute))
			if sleepDuration < 0 {
				sleepDuration = 0
			}
			time.Sleep(sleepDuration)

			slog.Info("Refreshing anonymous Spotify token in background")
			body, err := s.getAccessTokenPayload(s.ctx, nil) // no cookies for anonymous
			if err != nil {
				slog.Error("Failed to refresh anonymous token, retrying in 30s", slog.Any("err", err))
				time.Sleep(30 * time.Second)
				continue
			}

			exp := parseExpiry(body)

			cacheMutex.Lock()
			tokenCache["anonymous"] = cachedTokenEntry{
				Token:  body,
				Expiry: exp,
			}
			cacheMutex.Unlock()

			slog.Info("Anonymous Spotify token refreshed successfully", slog.Time("expiry", exp))
		}
	}()
}


func parseExpiry(body []byte) time.Time {
	expiry := time.Now().Add(55 * time.Minute) // fallback default

	var resp spotifyTokenResponse
	if err := json.Unmarshal(body, &resp); err == nil && resp.AccessTokenExpirationMillis > 0 {
		t := time.UnixMilli(resp.AccessTokenExpirationMillis)
		if t.After(time.Now()) {
			expiry = t
		}
	}

	return expiry
}

func cookiesKey(cookies []*network.CookieParam) string {
	if len(cookies) == 0 {
		return "anonymous"
	}
	var parts []string
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, ";")
}