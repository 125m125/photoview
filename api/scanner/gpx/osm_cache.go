package gpx

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/photoview/photoview/api/utils"
)

// OSM requires cross-render tile reuse and conditional revalidation. Keep this
// provider cache separate from the unchanged lifetime of media preview records.
// Serialize OSM requests so concurrent album scans cannot download the same tile.
var osmTileMutex sync.Mutex

type osmCachedTile struct {
	Data          []byte
	Expires       time.Time
	ETag          string
	LastModified  string
	CacheControl  string
	ExpiresHeader string
}

func osmCachePath(z, x, y int) string {
	return filepath.Join(utils.MediaCachePath(), "gpx-tiles", "osm", strconv.Itoa(z), strconv.Itoa(x), strconv.Itoa(y)+".json")
}

func readOSMTile(filename string) *osmCachedTile {
	file, err := os.Open(filename)
	if err != nil {
		return nil
	}
	defer file.Close()
	var tile osmCachedTile
	// Tile bodies have a 2 MiB limit; JSON base64 adds one third.
	if err := json.NewDecoder(io.LimitReader(file, 3<<20)).Decode(&tile); err != nil || len(tile.Data) == 0 || len(tile.Data) > 2<<20 {
		return nil
	}
	return &tile
}

func writeOSMTile(filename string, tile *osmCachedTile) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return fmt.Errorf("cannot create GPX OSM tile cache: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(filename), ".tile-*")
	if err != nil {
		return fmt.Errorf("cannot write GPX OSM tile cache: %w", err)
	}
	defer os.Remove(file.Name())
	encodeErr := json.NewEncoder(file).Encode(tile)
	closeErr := file.Close()
	if encodeErr != nil {
		return encodeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), filename)
}

// Honour origin freshness headers, including already-aged proxy responses. If no
// usable freshness information is supplied, OSM requires at least seven days.
func osmCacheExpiry(header http.Header, now time.Time) (expires time.Time, store bool) {
	expires = now.Add(7 * 24 * time.Hour)
	revalidate := false
	maxAge := -1
	for _, directive := range strings.Split(strings.Join(header.Values("Cache-Control"), ","), ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(directive), "=")
		switch strings.ToLower(key) {
		case "no-store":
			return now, false
		case "no-cache":
			revalidate = true
		case "max-age":
			seconds, err := strconv.Atoi(strings.Trim(value, `"`))
			if err == nil && seconds >= 0 {
				maxAge = seconds
			}
		}
	}
	date, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		date = now
	}
	ageSeconds, _ := strconv.Atoi(header.Get("Age"))
	// Avoid duration overflow from an invalid or hostile Age field.
	ageSeconds = min(max(0, ageSeconds), 365*24*60*60)
	age := max(time.Duration(ageSeconds)*time.Second, now.Sub(date))
	if maxAge >= 0 {
		lifetime := time.Duration(min(maxAge, 365*24*60*60)) * time.Second
		expires = now.Add(max(time.Duration(0), lifetime-age))
	} else if len(header.Values("Expires")) > 0 {
		value, err := http.ParseTime(header.Get("Expires"))
		if err != nil {
			expires = now
		} else {
			expires = now.Add(max(time.Duration(0), value.Sub(date)-age))
		}
	}

	if revalidate {
		expires = now
	}
	return expires, true
}
