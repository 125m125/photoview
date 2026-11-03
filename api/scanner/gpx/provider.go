package gpx

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/photoview/photoview/api/utils"
)

// A deliberately small XYZ raster provider. Configuration is server-only and
// opt-in: many public map APIs do not license permanently cached derivatives.
type tileProvider struct {
	template, attribution, userAgent string
	osm                              bool
	transport                        http.RoundTripper
	maxZoom                          int
}

const osmTileURL = "https://tile.openstreetmap.org/{z}/{x}/{y}.png"
const defaultUserAgent = "Photoview-GPX/1.0 (+https://github.com/125m125/photoview)"

func providerFromEnv() (tileProvider, error) {
	p := tileProvider{template: utils.EnvGPXTileURL.GetValue(), attribution: strings.TrimSpace(utils.EnvGPXAttribution.GetValue()), maxZoom: 16, userAgent: strings.TrimSpace(utils.EnvGPXUserAgent.GetValue())}
	if p.userAgent == "" {
		p.userAgent = defaultUserAgent
	}
	if strings.ContainsAny(p.userAgent, "\r\n") {
		return p, fmt.Errorf("GPX User-Agent must be a single HTTP header value")
	}
	p.osm = p.template == osmTileURL
	if p.osm {
		p.attribution = "© OpenStreetMap contributors https://www.openstreetmap.org/copyright"
	}
	if p.template == "" || p.attribution == "" {
		return p, fmt.Errorf("GPX previews require PHOTOVIEW_GPX_TILE_URL and PHOTOVIEW_GPX_ATTRIBUTION (see README)")
	}
	for _, key := range []string{"{z}", "{x}", "{y}"} {
		if !strings.Contains(p.template, key) {
			return p, fmt.Errorf("GPX tile URL must contain {z}, {x}, {y}")
		}
	}
	u, err := url.Parse(strings.NewReplacer("{z}", "0", "{x}", "0", "{y}", "0").Replace(p.template))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return p, fmt.Errorf("invalid GPX tile URL (use HTTP(S), without userinfo)")
	}
	host := strings.ToLower(u.Hostname())
	if (host == "tile.openstreetmap.org" || strings.HasSuffix(host, ".tile.openstreetmap.org")) && !p.osm {
		return p, fmt.Errorf("OSM requires the canonical HTTPS XYZ tile endpoint (see README)")
	}

	if value := utils.EnvGPXTileMaxZoom.GetValue(); value != "" {
		z, err := strconv.Atoi(value)
		if err != nil || z < 0 || z > 22 {
			return p, fmt.Errorf("GPX tile maximum zoom must be between 0 and 22")
		}
		p.maxZoom = z
	}
	if p.osm {
		p.transport, err = newOSMTransport()
		if err != nil {
			return p, err
		}
	}
	return p, nil
}

func (p tileProvider) background(ctx context.Context, v viewport) (*image.RGBA, error) {
	img := image.NewRGBA(image.Rect(0, 0, width, mapHeight))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	n := 1 << v.zoom
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	// Cache only within this render, e.g. repeated world tiles at low zoom.
	tiles := map[image.Point]image.Image{}
	for y := int(math.Floor(v.top / 256)); y <= int(math.Floor((v.top+mapHeight-1)/256)); y++ {
		if y < 0 || y >= n {
			continue
		}
		for x := int(math.Floor(v.left / 256)); x <= int(math.Floor((v.left+width-1)/256)); x++ {
			wrappedX := ((x % n) + n) % n
			key := image.Pt(wrappedX, y)
			tile := tiles[key]
			if tile == nil {
				var err error
				tile, err = p.tile(ctx, client, v.zoom, wrappedX, y)
				if err != nil {
					return nil, err
				}
				tiles[key] = tile
			}
			origin := image.Pt(x*256-int(math.Round(v.left)), y*256-int(math.Round(v.top)))
			draw.Draw(img, image.Rectangle{Min: origin, Max: origin.Add(image.Pt(256, 256))}, tile, image.Point{}, draw.Src)
		}
	}
	return img, nil
}

func (p tileProvider) tile(ctx context.Context, client *http.Client, z, x, y int) (image.Image, error) {
	if p.osm {
		osmTileMutex.Lock()
		defer osmTileMutex.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cachedClient := *client
		cachedClient.Transport = p.transport
		client = &cachedClient
	}

	target := strings.NewReplacer("{z}", strconv.Itoa(z), "{x}", strconv.Itoa(x), "{y}", strconv.Itoa(y)).Replace(p.template)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid GPX tile request")
	}
	req.Header.Set("User-Agent", p.userAgent)
	resp, err := client.Do(req)
	// Do not wrap HTTP errors: they can contain the URL and API credentials.
	if err != nil {
		return nil, fmt.Errorf("GPX background request failed (provider, network, or cache error)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GPX background provider returned HTTP %d", resp.StatusCode)
	}
	const limit = 2 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(data) > limit {
		return nil, fmt.Errorf("GPX background tile response unreadable or too large")
	}
	tile, err := decodeTile(data)
	if err != nil {
		return nil, err
	}
	return tile, nil
}

func decodeTile(data []byte) (image.Image, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != 256 || config.Height != 256 {
		return nil, fmt.Errorf("GPX background provider must return 256x256 PNG or JPEG XYZ tiles")
	}
	tile, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("GPX background tile could not be decoded")
	}
	return tile, nil
}
