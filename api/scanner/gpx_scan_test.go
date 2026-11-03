package scanner_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/photoview/photoview/api/graphql/models"
	"github.com/photoview/photoview/api/scanner"
	"github.com/photoview/photoview/api/scanner/face_detection"
	"github.com/photoview/photoview/api/scanner/media_type"
	"github.com/photoview/photoview/api/scanner/scanner_cache"
	"github.com/photoview/photoview/api/scanner/scanner_task"
	"github.com/photoview/photoview/api/test_utils"
	"github.com/photoview/photoview/api/utils"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type gpxTransport func(*http.Request) (*http.Response, error)

func (f gpxTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type rejectGPXFaceDetector struct {
	face_detection.FaceDetector
	t *testing.T
}

func (d rejectGPXFaceDetector) DetectFaces(_ *gorm.DB, media *models.Media) error {
	d.t.Errorf("GPX should never be submitted for face detection: %s", media.Path)
	return nil
}

func TestGPXScanAndCache(t *testing.T) {
	db := test_utils.DatabaseTest(t)
	previousDetector := face_detection.GlobalFaceDetector
	face_detection.GlobalFaceDetector = rejectGPXFaceDetector{t: t}
	defer func() { face_detection.GlobalFaceDetector = previousDetector }()
	previousCache := utils.MediaCachePath()
	utils.ConfigureTestCache(t.TempDir())
	defer utils.ConfigureTestCache(previousCache)
	// GPX is independent of the RAW processing option and of same-name JPEGs/XMP.
	t.Setenv("PHOTOVIEW_DISABLE_RAW_PROCESSING", "1")
	t.Setenv("PHOTOVIEW_GPX_TILE_URL", "https://synthetic.test/{z}/{x}/{y}.png")
	t.Setenv("PHOTOVIEW_GPX_ATTRIBUTION", "© Synthetic contributors")
	var requests int
	var tile bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{220, 230, 240, 255}), image.Point{}, draw.Src)
	require.NoError(t, png.Encode(&tile, img))
	previousTransport := http.DefaultTransport
	http.DefaultTransport = gpxTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(tile.Bytes())), Header: make(http.Header)}, nil
	})
	defer func() { http.DefaultTransport = previousTransport }()
	root := t.TempDir()
	data := []byte(`<gpx><trk><trkseg><trkpt lat="51" lon="10"/><trkpt lat="51.1" lon="10.1"/></trkseg></trk></gpx>`)
	for _, name := range []string{"day.GPX", "other.gPx"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), data, 0600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "day.GPX.xmp"), []byte("invalid sidecar, must be ignored"), 0600))
	album := models.Album{Title: "synthetic", Path: root}
	require.NoError(t, db.Create(&album).Error)
	scan := func() {
		cache := scanner_cache.MakeAlbumCache()
		cache.InsertAlbumIgnore(album.Path, nil)
		ctx := scanner_task.NewTaskContext(context.Background(), db, &album, cache)
		require.NoError(t, scanner.ScanAlbum(ctx))
	}
	scan()
	var media []models.Media
	require.NoError(t, db.Preload("MediaURL").Find(&media).Error)
	require.Len(t, media, 2)
	require.Greater(t, requests, 0)
	for _, m := range media {
		require.Equal(t, album.ID, m.AlbumID)
		require.Equal(t, models.MediaTypePhoto, m.Type)
		require.Nil(t, m.ExifID)
		require.Nil(t, m.SideCarPath)
		require.Nil(t, m.SideCarHash)
		require.Len(t, m.MediaURL, 3)
		var purposes []models.MediaPurpose
		for _, u := range m.MediaURL {
			purposes = append(purposes, u.Purpose)
			require.Positive(t, u.FileSize)
			require.Positive(t, u.Width)
			if u.Purpose == models.MediaOriginal {
				require.Equal(t, "application/gpx+xml", u.ContentType)
			}
		}
		require.ElementsMatch(t, []models.MediaPurpose{models.MediaOriginal, models.PhotoHighRes, models.PhotoThumbnail}, purposes)
		content, err := os.ReadFile(m.Path)
		require.NoError(t, err)
		require.Equal(t, data, content)
	}
	before := requests
	// Cached scans work even after provider configuration has been removed.
	t.Setenv("PHOTOVIEW_GPX_TILE_URL", "")
	scan()
	require.Equal(t, before, requests)
	thumb, err := media[0].GetThumbnail()
	require.NoError(t, err)
	thumbPath, err := thumb.CachedPath()
	require.NoError(t, err)
	require.NoError(t, os.Remove(thumbPath))
	scan()
	require.Equal(t, before, requests)
	_, err = os.Stat(thumbPath)
	require.NoError(t, err)
	// Normal rescanning also regenerates a missing high-res map.
	high, err := media[0].GetHighRes()
	require.NoError(t, err)
	highPath, err := high.CachedPath()
	require.NoError(t, err)
	require.NoError(t, os.Remove(highPath))
	t.Setenv("PHOTOVIEW_GPX_TILE_URL", "https://synthetic.test/{z}/{x}/{y}.png")
	scan()
	require.Greater(t, requests, before)
	_, err = os.Stat(highPath)
	require.NoError(t, err)
	var count int64
	require.NoError(t, db.Model(&models.MediaURL{}).Count(&count).Error)
	require.EqualValues(t, 6, count)
}

func TestGPXDiscoveryWithoutExif(t *testing.T) {
	for _, extension := range []string{"gpx", "GPX", "gPx"} {
		// There is deliberately no file or EXIF initialization needed to classify GPX.
		mt := media_type.GetMediaType("synthetic." + extension)
		require.Equal(t, "application/gpx+xml", mt.String())
		require.True(t, mt.IsSupported())
		require.False(t, mt.IsWebCompatible())
		require.False(t, mt.IsImage())
	}
	// A same-name photo must remain a separate media item.
	dir := t.TempDir()
	gpx := filepath.Join(dir, "day.gpx")
	photo := filepath.Join(dir, "day.jpg")
	require.NoError(t, os.WriteFile(gpx, []byte(`<gpx/>`), 0600))
	require.NoError(t, os.WriteFile(photo, []byte("image placeholder"), 0600))
	_, found := media_type.FindRawCounterpart(photo)
	require.False(t, found)
}
