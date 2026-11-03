package gpx

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/photoview/photoview/api/test_utils/flags"
	"github.com/stretchr/testify/require"
	"gopkg.in/gographics/imagick.v3/imagick"
)

func TestMain(m *testing.M) {
	imagick.Initialize()
	code := m.Run()
	imagick.Terminate()
	os.Exit(code)
}

func TestParseSegments(t *testing.T) {
	segments, err := parse(strings.NewReader(`<gpx xmlns="http://www.topografix.com/GPX/1/1"><trk><trkseg><trkpt lat="0" lon="0"/><trkpt lat="1" lon="2"/></trkseg><trkseg><trkpt lat="3" lon="4"/></trkseg></trk><trk><trkseg><trkpt lat="5" lon="6"/><trkpt lat="NaN" lon="0"/><trkpt lat="7" lon="8"/></trkseg></trk></gpx>`))
	require.NoError(t, err)
	require.Len(t, segments, 4)
	require.Len(t, segments[0], 2)
	require.Equal(t, point{Lat: 7, Lon: 8}, segments[3][0])
}

func TestParseBadInputs(t *testing.T) {
	for _, input := range []string{"", `<gpx/>`, `<gpx><trk>`, `<other/>`, `<gpx><trk><trkseg><trkpt lat="91" lon="0"/><trkpt lat="0" lon="181"/><trkpt lon="0"/></trkseg></trk></gpx>`, `<gpx/><gpx/>`, `<gpx><extensions><trk><trkseg><trkpt lat="0" lon="0"/></trkseg></trk></extensions></gpx>`} {
		t.Run(input, func(t *testing.T) { _, err := parse(strings.NewReader(input)); require.Error(t, err) })
	}
}

func TestLargeTrack(t *testing.T) {
	var input strings.Builder
	input.WriteString(`<gpx><trk><trkseg>`)
	for i := 0; i < 100000; i++ {
		fmt.Fprintf(&input, `<trkpt lat="%f" lon="%f"/>`, 50+float64(i)/1000000, 10+float64(i)/1000000)
	}
	input.WriteString(`</trkseg></trk></gpx>`)
	segments, err := parse(strings.NewReader(input.String()))
	require.NoError(t, err)
	require.Len(t, segments[0], 100000)
	view := fit(segments, 16)
	for _, p := range []point{segments[0][0], segments[0][99999]} {
		x, y := view.pixel(p)
		require.GreaterOrEqual(t, x, 32.0)
		require.LessOrEqual(t, x, 992.0)
		require.GreaterOrEqual(t, y, 32.0)
		require.LessOrEqual(t, y, 736.0)
	}
	server := mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, png.Encode(w, image.NewGray(image.Rect(0, 0, 256, 256))))
	}))
	t.Setenv("PHOTOVIEW_GPX_TILE_URL", server+"/{z}/{x}/{y}.png")
	t.Setenv("PHOTOVIEW_GPX_ATTRIBUTION", "Synthetic map")
	inputPath := filepath.Join(t.TempDir(), "large.gpx")
	require.NoError(t, os.WriteFile(inputPath, []byte(input.String()), 0600))
	require.NoError(t, RenderFile(context.Background(), inputPath, inputPath+".jpg"))

}

func TestAntimeridianAndPoles(t *testing.T) {
	for _, points := range [][]point{{{Lat: 0, Lon: 179.9}, {Lat: 0, Lon: -179.9}}, {{Lat: 90, Lon: 0}, {Lat: 89, Lon: 0}}, {{Lat: -90, Lon: 180}}} {
		view := fit([][]point{points}, 16)
		require.Greater(t, view.zoom, 0)
		for _, p := range points {
			x, y := view.pixel(p)
			require.True(t, x >= 32 && x <= 992 && y >= 32 && y <= 736, "%v: %v,%v", p, x, y)
		}
	}
}

func TestRenderFile(t *testing.T) {
	var requests int
	server := mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Regexp(t, `^/[0-9]+/[0-9]+/[0-9]+\.png$`, r.URL.Path)
		require.Empty(t, r.URL.RawQuery)
		tile := image.NewRGBA(image.Rect(0, 0, 256, 256))
		draw.Draw(tile, tile.Bounds(), image.NewUniform(color.RGBA{220, 230, 240, 255}), image.Point{}, draw.Src)
		require.NoError(t, png.Encode(w, tile))
	}))
	t.Setenv("PHOTOVIEW_GPX_TILE_URL", server+"/{z}/{x}/{y}.png")
	t.Setenv("PHOTOVIEW_GPX_ATTRIBUTION", "© Synthetic map contributors https://example.test")
	input := filepath.Join(t.TempDir(), "track.gpx")
	output := input + ".jpg"
	require.NoError(t, os.WriteFile(input, []byte(`<gpx><trk><trkseg><trkpt lat="0" lon="-1"/><trkpt lat="0" lon="1"/></trkseg><trkseg><trkpt lat="1" lon="-1"/><trkpt lat="1" lon="1"/></trkseg></trk></gpx>`), 0600))
	require.NoError(t, RenderFile(context.Background(), input, output))
	require.Greater(t, requests, 0)
	require.LessOrEqual(t, requests, 20)
	f, err := os.Open(output)
	require.NoError(t, err)
	defer f.Close()
	img, err := jpeg.Decode(f)
	require.NoError(t, err)
	require.Equal(t, 1024, img.Bounds().Dx())
	require.Greater(t, img.Bounds().Dy(), 768)
	// A segment joins neither the two parallel tracks nor their start/end markers.
	r, g, b, _ := img.At(512, 384).RGBA()
	require.Less(t, r, g)
	require.Less(t, g, b)
	for _, y := range []int{202, 566} {
		r, g, b, _ := img.At(512, y).RGBA()
		require.Greater(t, r, g*2)
		require.Greater(t, r, b*2)
	}

	// Attribution is embedded as dark glyphs on the white footer.
	dark := 0
	for y := 768; y < img.Bounds().Dy(); y++ {
		for x := 0; x < 1024; x++ {
			r, _, _, _ := img.At(x, y).RGBA()
			if r < 20000 {
				dark++
			}
		}
	}
	require.Greater(t, dark, 100)
}

func TestProviderFailuresDoNotWritePreview(t *testing.T) {
	for _, mode := range []string{"missing", "attribution", "status", "invalid image", "oversize image"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			server := mockTiles(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				switch mode {
				case "status":
					w.WriteHeader(429)
				case "invalid image":
					fmt.Fprint(w, "broken")
				default:
					png.Encode(w, image.NewGray(image.Rect(0, 0, 257, 256)))
				}
			}))
			t.Setenv("PHOTOVIEW_GPX_TILE_URL", server+"/{z}/{x}/{y}?key=secret-value")
			t.Setenv("PHOTOVIEW_GPX_ATTRIBUTION", "Synthetic")
			if mode == "missing" {
				t.Setenv("PHOTOVIEW_GPX_TILE_URL", "")
			}
			if mode == "attribution" {
				t.Setenv("PHOTOVIEW_GPX_ATTRIBUTION", "")
			}
			input := filepath.Join(t.TempDir(), "track.gpx")
			require.NoError(t, os.WriteFile(input, []byte(`<gpx><trk><trkseg><trkpt lat="0" lon="0"/></trkseg></trk></gpx>`), 0600))
			err := RenderFile(context.Background(), input, input+".jpg")
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret-value")
			if mode == "missing" || mode == "attribution" {
				require.Zero(t, requests)
			} else {
				require.Equal(t, 1, requests, "provider errors must not be retried")
			}
			_, err = os.Stat(input + ".jpg")
			require.True(t, os.IsNotExist(err))
		})
	}
}

// An HTTP transport fixture avoids external access and even local socket requirements.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockTiles(t *testing.T, handler http.Handler) string {
	original := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	return "https://synthetic.test"
}
