package routes

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gorilla/mux"
	"gorm.io/gorm"

	"github.com/photoview/photoview/api/graphql/models"
	"github.com/photoview/photoview/api/log"
	"github.com/photoview/photoview/api/scanner"
	"github.com/photoview/photoview/api/scanner/media_type"
)

func RegisterPhotoRoutes(db *gorm.DB, router *mux.Router) {

	router.HandleFunc("/{name}", func(w http.ResponseWriter, r *http.Request) {
		mediaName := mux.Vars(r)["name"]

		var mediaURL models.MediaURL
		result := db.Model(&models.MediaURL{}).Joins("Media").Select("media_urls.*").Where("media_urls.media_name = ?", mediaName).Scan(&mediaURL)
		if err := result.Error; err != nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("404"))
			return
		}

		media := mediaURL.Media
		if media == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("404 - Media not found"))
			return
		}

		if success, response, status, err := authenticateMedia(media, db, r); !success {
			if err != nil {
				log.Warn(r.Context(), "Unauthorized access to photo", "reason", err)
			}
			w.WriteHeader(status)
			w.Write([]byte(response))
			return
		}

		cachedPath, err := mediaURL.CachedPath()
		if err != nil {
			log.Error(r.Context(), "error getting cached path for media URL", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(internalServerError))
			return
		}

		if _, err := os.Stat(cachedPath); os.IsNotExist((err)) {
			// GPX composition is scan-time only. A rescan repairs missing maps.
			if media_type.IsGPX(media.Path) {
				http.NotFound(w, r)
				return
			}
			// err := db.Transaction(func(tx *gorm.DB) error {
			if err = scanner.ProcessSingleMediaFunc(r.Context(), db, media); err != nil {
				log.Error(r.Context(), "processing image not found in cache",
					"media_cache_path", cachedPath,
					"error", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(internalServerError))
				return
			}

			if _, err = os.Stat(cachedPath); err != nil {
				log.Error(r.Context(), "after reprocessing image not found in cache",
					"media_cache_path", cachedPath,
					"error", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(internalServerError))
				return
			}
		}

		// Allow caching the resource
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		if mediaURL.ContentType != "" {
			w.Header().Set("Content-Type", mediaURL.ContentType)
		}

		if mediaURL.Purpose == models.MediaOriginal && media_type.IsGPX(media.Path) {
			w.Header().Set("Content-Type", "application/gpx+xml")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(media.Path)}))
			w.Header().Set("X-Content-Type-Options", "nosniff")
		}
		http.ServeFile(w, r, cachedPath)
	})
}
