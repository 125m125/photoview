package routes

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/photoview/photoview/api/graphql/auth"
	"github.com/photoview/photoview/api/graphql/models"
	"github.com/photoview/photoview/api/scanner"
	"github.com/photoview/photoview/api/test_utils"
	"github.com/photoview/photoview/api/utils"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGPXDownloadAndAuthorization(t *testing.T) {
	db := test_utils.DatabaseTest(t)
	previousCache := utils.MediaCachePath()
	utils.ConfigureTestCache(t.TempDir())
	defer utils.ConfigureTestCache(previousCache)
	owner, err := models.RegisterUser(db, "gpx-owner", nil, false)
	require.NoError(t, err)
	outsider, err := models.RegisterUser(db, "outsider", nil, false)
	require.NoError(t, err)
	album := models.Album{Title: "GPX", Path: t.TempDir()}
	require.NoError(t, db.Model(owner).Association("Albums").Append(&album))
	original := []byte("<?xml version=\"1.0\"?>\r\n<gpx><trk><trkseg><trkpt lat=\"0\" lon=\"0\"/></trkseg></trk></gpx>\r\n")
	media := models.Media{Title: "Morning walk ü.GPX", Path: filepath.Join(album.Path, "Morning walk ü.GPX"), AlbumID: album.ID, Type: models.MediaTypePhoto, DateShot: time.Now()}
	require.NoError(t, os.WriteFile(media.Path, original, 0600))
	require.NoError(t, db.Create(&media).Error)
	for _, entry := range []struct {
		name    string
		purpose models.MediaPurpose
		mime    string
	}{{"random.GPX", models.MediaOriginal, "application/gpx+xml"}, {"map.jpg", models.PhotoHighRes, "image/jpeg"}, {"thumb.jpg", models.PhotoThumbnail, "image/jpeg"}} {
		u := models.MediaURL{MediaID: media.ID, Media: &media, MediaName: entry.name, Purpose: entry.purpose, ContentType: entry.mime, Width: 1024, Height: 796, FileSize: 100}
		require.NoError(t, db.Create(&u).Error)
		if entry.purpose != models.MediaOriginal {
			p, err := u.CachedPath()
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0700))
			require.NoError(t, os.WriteFile(p, []byte("synthetic-map"), 0600))
		}
	}
	share := models.ShareToken{Value: "gpx-share", AlbumID: &album.ID, OwnerID: owner.ID}
	require.NoError(t, db.Create(&share).Error)
	otherAlbum := models.Album{Title: "Other", Path: t.TempDir()}
	require.NoError(t, db.Create(&otherAlbum).Error)
	wrongShare := models.ShareToken{Value: "wrong-share", AlbumID: &otherAlbum.ID, OwnerID: owner.ID}
	require.NoError(t, db.Create(&wrongShare).Error)
	expiredTime := time.Now().Add(-time.Hour)
	expiredShare := models.ShareToken{Value: "expired", MediaID: &media.ID, OwnerID: owner.ID, Expire: &expiredTime}
	require.NoError(t, db.Create(&expiredShare).Error)
	router := mux.NewRouter()
	RegisterPhotoRoutes(db, router)
	for _, name := range []string{"random.GPX", "map.jpg", "thumb.jpg"} {
		for _, tc := range []struct {
			name, query string
			user        *models.User
			status      int
		}{{"owner", "", owner, 200}, {"outsider", "", outsider, 403}, {"anonymous", "", nil, 403}, {"valid share", "?token=gpx-share", nil, 200}, {"wrong share", "?token=wrong-share", nil, 403}, {"expired share", "?token=expired", nil, 403}} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequest("GET", "/"+name+tc.query, nil)
				if tc.user != nil {
					req = req.WithContext(auth.AddUserToContext(req.Context(), tc.user))
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				require.Equal(t, tc.status, rec.Code)
				if tc.status != 200 {
					require.NotContains(t, rec.Body.String(), "<gpx>")
					require.NotContains(t, rec.Body.String(), "synthetic-map")
					return
				}
				if name == "random.GPX" {
					require.Equal(t, original, rec.Body.Bytes())
					require.Equal(t, "application/gpx+xml", rec.Header().Get("Content-Type"))
					disposition, params, err := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
					require.NoError(t, err)
					require.Equal(t, "attachment", disposition)
					require.Equal(t, media.Title, params["filename"])
				} else {
					require.Equal(t, "synthetic-map", rec.Body.String())
				}
			})
		}
	}
	// Album ZIP downloads also preserve the original bytes and filename.
	downloads := mux.NewRouter()
	RegisterDownloadRoutes(db, downloads)
	for _, user := range []*models.User{owner, outsider, nil} {
		req := httptest.NewRequest("GET", fmt.Sprintf("/album/%d/original", album.ID), nil)
		if user != nil {
			req = req.WithContext(auth.AddUserToContext(req.Context(), user))
		}
		rec := httptest.NewRecorder()
		downloads.ServeHTTP(rec, req)
		if user != owner {
			require.Equal(t, 403, rec.Code)
			continue
		}
		require.Equal(t, 200, rec.Code)
		archive, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
		require.NoError(t, err)
		require.Len(t, archive.File, 1)
		require.Equal(t, "GPX/Morning walk ü.GPX", archive.File[0].Name)
		entry, err := archive.File[0].Open()
		require.NoError(t, err)
		content, err := io.ReadAll(entry)
		require.NoError(t, err)
		require.NoError(t, entry.Close())
		require.Equal(t, original, content)
	}

	// Serving a missing GPX map never invokes the map renderer on an HTTP request.
	scanCalls := 0
	previousScan := scanner.ProcessSingleMediaFunc
	scanner.ProcessSingleMediaFunc = func(context.Context, *gorm.DB, *models.Media) error { scanCalls++; return nil }
	defer func() { scanner.ProcessSingleMediaFunc = previousScan }()
	cache, err := media.CachePath()
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(cache, "map.jpg")))
	req := httptest.NewRequest("GET", "/map.jpg", nil)
	req = req.WithContext(auth.AddUserToContext(req.Context(), owner))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, 404, rec.Code)
	require.Zero(t, scanCalls)
}
