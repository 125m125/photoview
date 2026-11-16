// Package gpx generates cached static map previews without sending tracks to a map service.
package gpx

import (
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

type point struct{ Lat, Lon float64 }

// parse streams XML, retaining only valid track coordinates. Invalid points break
// a segment so missing data never creates an artificial connecting line.
func parse(r io.Reader) ([][]point, error) {
	decoder := xml.NewDecoder(r)
	var stack []string
	var segments [][]point
	var segment []point
	rootSeen := false
	flush := func() {
		if len(segment) > 0 {
			segments = append(segments, segment)
			segment = nil
		}
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("malformed GPX: %w", err)
		}
		switch elem := token.(type) {
		case xml.StartElement:
			if len(stack) == 0 {
				if rootSeen || elem.Name.Local != "gpx" || (elem.Name.Space != "" && elem.Name.Space != "http://www.topografix.com/GPX/1/1" && elem.Name.Space != "http://www.topografix.com/GPX/1/0") {
					return nil, fmt.Errorf("expected one GPX document")
				}
				rootSeen = true
			}
			stack = append(stack, elem.Name.Local)
			if strings.Join(stack, "/") == "gpx/trk/trkseg/trkpt" {
				lat, lon := math.NaN(), math.NaN()
				for _, attr := range elem.Attr {
					if attr.Name.Space != "" {
						continue
					}
					switch attr.Name.Local {
					case "lat":
						var err error
						lat, err = strconv.ParseFloat(attr.Value, 64)
						if err != nil {
							lat = math.NaN()
						}
					case "lon":
						var err error
						lon, err = strconv.ParseFloat(attr.Value, 64)
						if err != nil {
							lon = math.NaN()
						}
					}
				}
				if math.IsNaN(lat) || math.IsNaN(lon) || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
					flush()
					continue
				}
				segment = append(segment, point{Lat: lat, Lon: lon})
			}
		case xml.EndElement:
			if strings.Join(stack, "/") == "gpx/trk/trkseg" {
				flush()
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 && strings.TrimSpace(string(elem)) != "" {
				return nil, fmt.Errorf("text outside GPX document")
			}
		}
	}
	if !rootSeen || len(segments) == 0 {
		return nil, fmt.Errorf("GPX contains no valid track points")
	}
	return segments, nil
}
