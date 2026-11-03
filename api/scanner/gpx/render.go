package gpx

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/gographics/imagick.v3/imagick"
)

const width, mapHeight, padding = 1024, 768, 40

type viewport struct {
	zoom           int
	left, top, cut float64
}

func project(p point) (float64, float64) {
	lat := math.Max(-85.05112878, math.Min(85.05112878, p.Lat)) * math.Pi / 180
	return (p.Lon + 180) / 360, math.Max(0, math.Min(1, (1-math.Asinh(math.Tan(lat))/math.Pi)/2))
}

// fit removes the largest unused longitude gap, including across the dateline.
func fit(segments [][]point, maxZoom int) viewport {
	xs := make([]float64, 0)
	minY, maxY := 1.0, 0.0
	for _, s := range segments {
		for _, p := range s {
			x, y := project(p)
			xs = append(xs, x)
			minY = math.Min(minY, y)
			maxY = math.Max(maxY, y)
		}
	}
	sort.Float64s(xs)
	cut, gap := xs[0], xs[0]+1-xs[len(xs)-1]
	for i := 1; i < len(xs); i++ {
		if xs[i]-xs[i-1] > gap {
			gap = xs[i] - xs[i-1]
			cut = xs[i]
		}
	}
	minX, maxX := cut, cut+1-gap
	zoom := maxZoom
	for zoom > 0 {
		scale := 256 * math.Exp2(float64(zoom))
		if (maxX-minX)*scale <= width-2*padding && (maxY-minY)*scale <= mapHeight-2*padding {
			break
		}
		zoom--
	}
	scale := 256 * math.Exp2(float64(zoom))
	return viewport{zoom: zoom, cut: cut, left: (minX+maxX)/2*scale - width/2, top: (minY+maxY)/2*scale - mapHeight/2}
}

func (v viewport) pixel(p point) (float64, float64) {
	x, y := project(p)
	if x < v.cut {
		x++
	}
	scale := 256 * math.Exp2(float64(v.zoom))
	return x*scale - v.left, y*scale - v.top
}

// RenderFile creates a complete JPEG atomically; failed generation never leaves
// a partial preview which a later scan could mistake for a cached image.
func RenderFile(ctx context.Context, inputPath, outputPath string) error {
	file, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer file.Close()
	segments, err := parse(file)
	if err != nil {
		return err
	}
	provider, err := providerFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	view := fit(segments, provider.maxZoom)
	img, err := provider.background(ctx, view)
	if err != nil {
		return err
	}
	trackColor := color.RGBA{210, 35, 45, 255}
	for _, segment := range segments {
		if err := ctx.Err(); err != nil {
			return err
		}
		x, y := view.pixel(segment[0])
		prev := image.Pt(int(math.Round(x)), int(math.Round(y)))
		circle(img, prev, 3, trackColor)
		for _, p := range segment[1:] {
			x, y = view.pixel(p)
			next := image.Pt(int(math.Round(x)), int(math.Round(y)))
			if next != prev {
				line(img, prev, next, trackColor)
			}
			prev = next
		}
		x, y = view.pixel(segment[0])
		circle(img, image.Pt(int(math.Round(x)), int(math.Round(y))), 7, color.RGBA{255, 255, 255, 255})
		circle(img, image.Pt(int(math.Round(x)), int(math.Round(y))), 5, color.RGBA{20, 150, 65, 255})
		circle(img, prev, 7, color.RGBA{255, 255, 255, 255})
		circle(img, prev, 4, trackColor)
	}
	result, err := attribute(img, provider.attribution)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(outputPath), ".gpx-*.jpg")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	err = jpeg.Encode(temp, result, &jpeg.Options{Quality: 90})
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), outputPath)
}

func circle(img *image.RGBA, p image.Point, r int, c color.RGBA) {
	for y := -r; y <= r; y++ {
		for x := -r; x <= r; x++ {
			if x*x+y*y <= r*r {
				img.SetRGBA(p.X+x, p.Y+y, c)
			}
		}
	}
}
func line(img *image.RGBA, a, b image.Point, c color.RGBA) {
	dx, dy := b.X-a.X, b.Y-a.Y
	steps := max(abs(dx), abs(dy))
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		circle(img, image.Pt(a.X+int(math.Round(float64(dx)*t)), a.Y+int(math.Round(float64(dy)*t))), 2, c)
	}
}
func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Attribution lives in a footer outside the map, so tracks cannot obscure it.
// At 1024px wide the normal thumbnail pipeline preserves its size and legibility.
func attribute(img *image.RGBA, text string) (image.Image, error) {
	wand := imagick.NewMagickWand()
	defer wand.Destroy()
	white := imagick.NewPixelWand()
	defer white.Destroy()
	white.SetColor("white")
	if err := wand.NewImage(width, mapHeight, white); err != nil {
		return nil, err
	}
	pen := imagick.NewDrawingWand()
	defer pen.Destroy()
	// Use a known font, installed with the runtime dependencies.
	if err := pen.SetFont("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"); err != nil {
		return nil, err
	}
	pen.SetFontSize(14)
	pen.SetTextEncoding("UTF-8")
	black := imagick.NewPixelWand()
	defer black.Destroy()
	black.SetColor("black")
	pen.SetFillColor(black)
	var lines []string
	current := ""
	for _, r := range text {
		metrics := wand.QueryFontMetrics(pen, current+string(r))
		if metrics == nil {
			return nil, fmt.Errorf("GPX attribution font unavailable; install fonts-dejavu-core and ImageMagick with FreeType")
		}
		if r == '\n' || metrics.TextWidth > width-16 {
			lines = append(lines, current)
			current = ""
			if r == '\n' {
				continue
			}
		}
		current += string(r)
	}
	if strings.TrimSpace(current) != "" {
		lines = append(lines, current)
	}
	if len(lines) > 16 {
		return nil, fmt.Errorf("GPX provider attribution is too long")
	}
	result := image.NewRGBA(image.Rect(0, 0, width, mapHeight+8+20*len(lines)))
	draw.Draw(result, result.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(result, img.Bounds(), img, image.Point{}, draw.Src)
	var data bytes.Buffer
	if err := png.Encode(&data, result); err != nil {
		return nil, err
	}
	if err := wand.ReadImageBlob(data.Bytes()); err != nil {
		return nil, err
	}
	for i, line := range lines {
		if err := wand.AnnotateImage(pen, 8, float64(mapHeight+18+i*20), 0, line); err != nil {
			return nil, err
		}
	}
	if err := wand.SetImageFormat("PNG"); err != nil {
		return nil, err
	}
	blob, err := wand.GetImageBlob()
	if err != nil {
		return nil, err
	}
	return png.Decode(bytes.NewReader(blob))
}
