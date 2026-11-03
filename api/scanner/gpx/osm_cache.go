package gpx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bartventer/httpcache"
	_ "github.com/bartventer/httpcache/store/fscache"
	"github.com/photoview/photoview/api/utils"
)

// Serialize OSM lookups/fetches across scans; HTTP cache semantics and disk
// storage belong to the transport rather than the GPX renderer.
var osmTileMutex sync.Mutex

func newOSMTransport() (transport http.RoundTripper, err error) {
	// The library reports cache initialization failures by panicking.
	defer func() {
		if value := recover(); value != nil {
			if cause, ok := value.(error); ok && errors.Is(cause, httpcache.ErrOpenCache) {
				err = fmt.Errorf("cannot open GPX OSM tile cache: %w", cause)
			} else {
				panic(value)
			}
		}
	}()
	base, err := filepath.Abs(filepath.Join(utils.MediaCachePath(), "gpx-tiles"))
	if err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "fscache", Path: base, RawQuery: "appname=osm-http&umask=077&timeout=5s&connect_timeout=5s"}
	return httpcache.NewTransport(dsn.String(), httpcache.WithUpstream(osmUpstream{})), nil
}

type osmUpstream struct{}

func (osmUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	// Normalize repeated fields before the cache transport reads them.
	control := strings.Join(resp.Header.Values("Cache-Control"), ",")
	if len(resp.Header.Values("Cache-Control")) > 0 {
		resp.Header.Set("Cache-Control", control)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
		resp.Body.Close()
		return nil, fmt.Errorf("GPX background provider returned HTTP %d", resp.StatusCode)
	}
	// Do not pass uncacheable 304 updates into the library's unconditional merge.
	if resp.StatusCode == http.StatusNotModified {
		for _, directive := range strings.Split(control, ",") {
			if strings.EqualFold(strings.TrimSpace(directive), "no-store") {
				resp.Body.Close()
				return nil, fmt.Errorf("GPX tile revalidation prohibits storage")
			}
		}
	}
	// Prefer synchronous validation to stale responses/background refreshes.
	if resp.StatusCode == http.StatusOK || control != "" {
		resp.Header.Set("Cache-Control", control+", must-revalidate")
	}
	// v0.14 can fall through to Expires/heuristic freshness for max-age=0.
	if strings.Contains(strings.ToLower(control), "max-age") {
		resp.Header.Set("Expires", "0")
	}
	if resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}

	// Validate and bound the body before the caching transport can persist it.
	defer resp.Body.Close()
	const limit = 2 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(data) > limit {
		return nil, fmt.Errorf("GPX background tile response unreadable or too large")
	}
	if _, err := decodeTile(data); err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	// OSM requires seven days when no freshness headers are supplied.
	if control == "" && len(resp.Header.Values("Expires")) == 0 {
		resp.Header.Set("Cache-Control", "max-age=604800, must-revalidate")
	}
	return resp, nil
}
