package relay

import (
	"bytes"
	_ "embed"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"sync"
	"time"

	"fncluster/internal/mesh"
)

//go:embed badge.png
var badgePNG []byte

const (
	// iconSize is the minimum composite output and the badge reference size.
	// Builtin fnOS icons are 224px; keeping the source resolution avoids the
	// browser upscale blur that made remote entries look soft.
	iconSize         = 64
	iconMaxSize      = 256
	badgeSize        = 22
	iconCacheTime    = 24 * time.Hour
	iconFallbackTime = 10 * time.Minute
	maxIconBytes     = 1 << 20
)

var (
	badgeOnce  sync.Once
	badgeImage image.Image
	badgeErr   error
)

func badge() (image.Image, error) {
	badgeOnce.Do(func() {
		badgeImage, badgeErr = png.Decode(bytes.NewReader(badgePNG))
	})
	return badgeImage, badgeErr
}

// DefaultIcon is served when the remote icon cannot be fetched.
func DefaultIcon() []byte {
	return badgePNG
}

type iconEntry struct {
	at   time.Time
	ttl  time.Duration
	data []byte
}

// Icon returns the remote app's icon composited with the FnCluster badge.
// Fallback order: remote icon -> local same-app icon -> cluster logo (no badge
// overlay on the logo itself). Fallback results expire quickly so a countable
// icon takes over later.
func (s *Server) Icon(nodeID, appName string) ([]byte, error) {
	key := nodeID + "|" + appName
	s.iconMu.Lock()
	if entry, ok := s.iconCache[key]; ok && time.Since(entry.at) < entry.ttl {
		s.iconMu.Unlock()
		return entry.data, nil
	}
	s.iconMu.Unlock()

	base, err := s.fetchIcon(nodeID, appName)
	fallback := err != nil
	if fallback && s.LocalIcon != nil {
		if local, ok := s.LocalIcon(appName); ok {
			base = local
			fallback = false
		}
	}
	var composed []byte
	ttl := iconCacheTime
	if fallback {
		composed = badgePNG
		ttl = iconFallbackTime
	} else {
		composed, err = compositeIcon(base)
		if err != nil {
			composed = base
		}
	}
	s.iconMu.Lock()
	if s.iconCache == nil {
		s.iconCache = map[string]iconEntry{}
	}
	s.iconCache[key] = iconEntry{at: time.Now(), ttl: ttl, data: composed}
	s.iconMu.Unlock()
	return composed, nil
}

func (s *Server) fetchIcon(nodeID, appName string) ([]byte, error) {
	store, err := mesh.Open(s.DataDir)
	if err != nil {
		return nil, err
	}
	cfg := store.Snapshot()
	address, ok := cfg.Members[nodeID]
	if !ok {
		return nil, errors.New("node is not a cluster member")
	}
	client, err := mesh.RelayClient(cfg, nodeID)
	if err != nil {
		return nil, err
	}
	client.Timeout = 5 * time.Second
	response, err := client.Get("https://" + address + "/icon/" + appName)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("remote icon unavailable")
	}
	return io.ReadAll(io.LimitReader(response.Body, maxIconBytes))
}

func compositeIcon(base []byte) ([]byte, error) {
	source, err := png.Decode(bytes.NewReader(base))
	if err != nil {
		return nil, err
	}
	bounds := source.Bounds()
	size := bounds.Dx()
	if bounds.Dy() > size {
		size = bounds.Dy()
	}
	if size < iconSize {
		size = iconSize
	}
	if size > iconMaxSize {
		size = iconMaxSize
	}
	target := scaleNearest(source, size)
	mark, err := badge()
	if err != nil {
		return nil, err
	}
	markSize := size * badgeSize / iconSize
	scaled := scaleNearest(mark, markSize)
	offset := size - markSize - 2
	white := image.NewUniform(color.RGBA{R: 255, G: 255, B: 255, A: 255})
	draw.Draw(target, image.Rect(offset-2, offset-2, offset+markSize+2, offset+markSize+2), white, image.Point{}, draw.Src)
	draw.Draw(target, image.Rect(offset, offset, offset+markSize, offset+markSize), scaled, image.Point{}, draw.Src)
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, target); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// scaleNearest resizes with nearest-neighbor sampling; stdlib only.
func scaleNearest(source image.Image, size int) *image.RGBA {
	target := image.NewRGBA(image.Rect(0, 0, size, size))
	bounds := source.Bounds()
	for y := 0; y < size; y++ {
		sourceY := bounds.Min.Y + y*bounds.Dy()/size
		for x := 0; x < size; x++ {
			sourceX := bounds.Min.X + x*bounds.Dx()/size
			target.Set(x, y, source.At(sourceX, sourceY))
		}
	}
	return target
}
