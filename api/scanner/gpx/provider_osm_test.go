package gpx

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/photoview/photoview/api/utils"
	"github.com/stretchr/testify/require"
)

const osmURL = "https://tile.openstreetmap.org/{z}/{x}/{y}.png"

func osmTestProvider(t *testing.T) tileProvider {
	t.Helper()
	previous := utils.GetTestCachePath()
	utils.ConfigureTestCache(t.TempDir())
	t.Cleanup(func() { utils.ConfigureTestCache(previous) })
	t.Setenv("PHOTOVIEW_GPX_TILE_URL", osmURL)
	t.Setenv("PHOTOVIEW_GPX_ATTRIBUTION", "")
	t.Setenv("PHOTOVIEW_GPX_USER_AGENT", "")
	p, err := providerFromEnv()
	require.NoError(t, err)
	require.Contains(t, p.attribution, "© OpenStreetMap contributors")
	require.Contains(t, p.attribution, "https://www.openstreetmap.org/copyright")
	return p
}

func respondOSMTile(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	tile := image.NewRGBA(image.Rect(0, 0, 256, 256))
	tile.SetRGBA(0, 0, color.RGBA{20, 40, 60, 255})
	require.NoError(t, png.Encode(w, tile))
}

func TestOSMTileIdentificationAndCacheReuse(t *testing.T) {
	p := osmTestProvider(t)
	requests := 0
	mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, "https://tile.openstreetmap.org/2/1/1.png", r.URL.String())
		require.Contains(t, r.UserAgent(), "Photoview-GPX/")
		require.Contains(t, r.UserAgent(), "https://github.com/125m125/photoview")
		require.Empty(t, r.Header.Get("Cache-Control"))
		require.Empty(t, r.Header.Get("Pragma"))
		w.Header().Set("Cache-Control", "public, max-age=3600")
		respondOSMTile(t, w)
	}))
	first, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
	require.NoError(t, err)
	// A new provider instance/render should reuse the on-disk cached tile.
	p, err = providerFromEnv()
	require.NoError(t, err)
	second, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
	require.NoError(t, err)
	require.Equal(t, first.At(0, 0), second.At(0, 0))
	require.Equal(t, 1, requests)
}

func TestOSMExpiredTilesUseConditionalRequests(t *testing.T) {
	p := osmTestProvider(t)
	requests := 0
	modified := "Wed, 01 Jan 2025 00:00:00 GMT"
	mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Cache-Control", "max-age=0")
			w.Header().Set("ETag", `"synthetic-v1"`)
			w.Header().Set("Last-Modified", modified)
			respondOSMTile(t, w)
			return
		}
		require.Equal(t, `"synthetic-v1"`, r.Header.Get("If-None-Match"))
		require.Equal(t, modified, r.Header.Get("If-Modified-Since"))
		w.Header().Set("Cache-Control", "max-age=3600")
		w.WriteHeader(http.StatusNotModified)
	}))
	for i := 0; i < 3; i++ {
		tile, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
		require.NoError(t, err)
		r, g, b, _ := tile.At(0, 0).RGBA()
		require.EqualValues(t, 20*257, r)
		require.EqualValues(t, 40*257, g)
		require.EqualValues(t, 60*257, b)
	}
	require.Equal(t, 2, requests)
}

func TestOSMCacheHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, cacheControl, expires string
		requests                    int
	}{
		{"fallback seven days", "", "", 1},
		{"Expires", "", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), 1},
		{"no store", "no-store", "", 2},
		{"no cache", "no-cache, max-age=3600", "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := osmTestProvider(t)
			requests := 0
			mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if tc.cacheControl != "" {
					w.Header().Set("Cache-Control", tc.cacheControl)
				}
				if tc.expires != "" {
					w.Header().Set("Expires", tc.expires)
				}
				respondOSMTile(t, w)
			}))
			for i := 0; i < 2; i++ {
				_, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
				require.NoError(t, err)
			}
			require.Equal(t, tc.requests, requests)
		})
	}
}

func TestOSMRejectsNonCanonicalEndpoint(t *testing.T) {
	for _, template := range []string{"http://tile.openstreetmap.org/{z}/{x}/{y}.png", "https://a.tile.openstreetmap.org/{z}/{x}/{y}.png", "https://tile.openstreetmap.org/{z}/{x}/{y}.png?key=secret"} {
		t.Run(template, func(t *testing.T) {
			t.Setenv("PHOTOVIEW_GPX_TILE_URL", template)
			t.Setenv("PHOTOVIEW_GPX_ATTRIBUTION", "Synthetic")
			_, err := providerFromEnv()
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestGPXOperatorUserAgent(t *testing.T) {
	osmTestProvider(t)
	t.Setenv("PHOTOVIEW_GPX_USER_AGENT", "Photoview-GPX/1.0 (+https://photos.example/contact)")
	p, err := providerFromEnv()
	require.NoError(t, err)
	mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Photoview-GPX/1.0 (+https://photos.example/contact)", r.UserAgent())
		respondOSMTile(t, w)
	}))
	_, err = p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
	require.NoError(t, err)
}

func TestOSMCacheDoesNotStoreFailedTiles(t *testing.T) {
	p := osmTestProvider(t)
	requests := 0
	mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Write(bytes.Repeat([]byte("invalid"), 10))
			return
		}
		respondOSMTile(t, w)
	}))
	_, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
	require.Error(t, err)
	_, err = p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
	require.NoError(t, err)
	require.Equal(t, 2, requests)
}

func TestOSMNotModifiedRetainsExpires(t *testing.T) {
	p := osmTestProvider(t)
	requests := 0
	mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Expires", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
			w.Header().Set("ETag", `"v1"`)
			respondOSMTile(t, w)
			return
		}
		require.Equal(t, `"v1"`, r.Header.Get("If-None-Match"))
		w.WriteHeader(http.StatusNotModified)
	}))
	for i := 0; i < 3; i++ {
		_, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
		require.NoError(t, err)
	}
	require.Equal(t, 3, requests, "304 without updated freshness must not grant an Expires-only tile a seven-day lifetime")
}

func TestOSMMultipleCacheControlHeaders(t *testing.T) {
	for _, directive := range []string{"no-store", "no-cache"} {
		t.Run(directive, func(t *testing.T) {
			p := osmTestProvider(t)
			requests := 0
			mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Add("Cache-Control", "max-age=3600")
				w.Header().Add("Cache-Control", directive)
				respondOSMTile(t, w)
			}))
			for i := 0; i < 2; i++ {
				_, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
				require.NoError(t, err)
			}
			require.Equal(t, 2, requests)
		})
	}
}

func TestOSMExpiresRespectsAgeAndInvalidValues(t *testing.T) {
	for _, expires := range []string{"0", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)} {
		t.Run(expires, func(t *testing.T) {
			p := osmTestProvider(t)
			requests := 0
			mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
				w.Header().Set("Expires", expires)
				w.Header().Set("Age", "7200")
				respondOSMTile(t, w)
			}))
			for i := 0; i < 2; i++ {
				_, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
				require.NoError(t, err)
			}
			require.Equal(t, 2, requests)
		})
	}
}

func TestOSMCacheUnavailableReturnsError(t *testing.T) {
	osmTestProvider(t)
	file := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(file, []byte("occupied"), 0600))
	utils.ConfigureTestCache(file)
	_, err := providerFromEnv()
	require.Error(t, err, "cache initialization should fail gracefully rather than panic")
}

func TestOSMMaxAgeOverridesExpires(t *testing.T) {
	for _, tc := range []struct {
		control  string
		expires  time.Time
		requests int
	}{
		{"max-age=3600", time.Now().Add(-time.Hour), 1},
		{"max-age=0", time.Now().Add(time.Hour), 2},
	} {
		t.Run(tc.control, func(t *testing.T) {
			p := osmTestProvider(t)
			requests := 0
			mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Cache-Control", tc.control)
				w.Header().Set("Expires", tc.expires.UTC().Format(http.TimeFormat))
				respondOSMTile(t, w)
			}))
			for i := 0; i < 2; i++ {
				_, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
				require.NoError(t, err)
			}
			require.Equal(t, tc.requests, requests)
		})
	}
}

func TestOSMRejectsNoStoreRevalidation(t *testing.T) {
	p := osmTestProvider(t)
	requests := 0
	mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Cache-Control", "max-age=0")
			w.Header().Set("ETag", `"v1"`)
			respondOSMTile(t, w)
			return
		}
		w.Header().Set("Cache-Control", "no-store, max-age=3600")
		w.WriteHeader(http.StatusNotModified)
	}))
	_, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err = p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
		require.Error(t, err, "uncacheable revalidation must not enter a persistent preview")
	}
	require.Equal(t, 3, requests)
}

func TestOSMDoesNotServeStaleWhileRevalidating(t *testing.T) {
	p := osmTestProvider(t)
	requests := 0
	mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Cache-Control", "max-age=0, stale-while-revalidate=60")
			respondOSMTile(t, w)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	_, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
	require.NoError(t, err)
	_, err = p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
	require.Error(t, err)
	require.Equal(t, 2, requests)
}

func TestOSMNotModifiedMaxAgeOverridesExpires(t *testing.T) {
	p := osmTestProvider(t)
	requests := 0
	mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Cache-Control", "max-age=0")
		w.Header().Set("ETag", `"v1"`)
		if requests == 1 {
			respondOSMTile(t, w)
			return
		}
		w.Header().Set("Expires", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusNotModified)
	}))
	for i := 0; i < 3; i++ {
		_, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
		require.NoError(t, err)
	}
	require.Equal(t, 3, requests)
}

// Error bodies must never be consumed by the caching transport.
type rejectReadBody struct{ reads, closes int }

func (b *rejectReadBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *rejectReadBody) Close() error             { b.closes++; return nil }
func TestOSMRejectsErrorBodyBeforeCaching(t *testing.T) {
	p := osmTestProvider(t)
	body := &rejectReadBody{}
	original := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: body, Request: r}, nil
	})
	defer func() { http.DefaultTransport = original }()
	_, err := p.tile(context.Background(), http.DefaultClient, 2, 1, 1)
	require.Error(t, err)
	require.Zero(t, body.reads)
	require.Equal(t, 1, body.closes)
}
