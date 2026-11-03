package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

var ErrUnsupported = errors.New("миниатюра для этого типа не поддерживается")

// Sizes — допустимые размеры миниатюр (по большей стороне).
var Sizes = map[int]bool{128: true, 256: true, 512: true, 1024: true}

const maxPixels = 120_000_000 // не декодируем картинки больше 120 Мп (защита памяти)

func IsImage(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp":
		return true
	}
	return false
}

func HasFFmpeg() bool { _, err := exec.LookPath("ffmpeg"); return err == nil }
func HasPdftoppm() bool {
	_, err := exec.LookPath("pdftoppm")
	return err == nil
}

// CanThumb — можно ли в принципе сделать миниатюру для типа.
func CanThumb(mime string) bool {
	switch {
	case IsImage(mime):
		return true
	case strings.HasPrefix(mime, "video/"):
		return HasFFmpeg()
	case mime == "application/pdf":
		return HasPdftoppm()
	}
	return false
}

// Thumbnail строит JPEG-миниатюру. src должен поддерживать Seek (блоб из хранилища).
func Thumbnail(ctx context.Context, src io.ReadSeeker, srcSize int64, mime string, size int, tmpDir string) ([]byte, error) {
	switch {
	case IsImage(mime):
		return imageThumb(src, mime, size)
	case strings.HasPrefix(mime, "video/"):
		if !HasFFmpeg() {
			return nil, ErrUnsupported
		}
		return externalThumb(ctx, src, srcSize, tmpDir, size, "video")
	case mime == "application/pdf":
		if !HasPdftoppm() {
			return nil, ErrUnsupported
		}
		return externalThumb(ctx, src, srcSize, tmpDir, size, "pdf")
	}
	return nil, ErrUnsupported
}

func imageThumb(src io.ReadSeeker, mime string, size int) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(src)
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > maxPixels {
		return nil, ErrUnsupported
	}
	orientation := 1
	if mime == "image/jpeg" {
		if _, err := src.Seek(0, io.SeekStart); err == nil {
			if ex, err := ReadExif(src, nil); err == nil && ex.Orientation > 0 {
				orientation = ex.Orientation
			}
		}
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(src)
	if err != nil {
		return nil, err
	}
	img = orient(scale(img, size), orientation)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flatten(img), &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// scale вписывает изображение в квадрат size×size (без увеличения маленьких).
func scale(img image.Image, size int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= size && h <= size {
		return img
	}
	nw, nh := size, size
	if w > h {
		nh = max(1, h*size/w)
	} else {
		nw = max(1, w*size/h)
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}

// flatten накладывает прозрачность на белый фон (JPEG не умеет альфа-канал).
func flatten(img image.Image) image.Image {
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Over)
	return dst
}

// orient поворачивает картинку согласно EXIF Orientation.
func orient(img image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	var dst *image.RGBA
	if o >= 5 {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	} else {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := img.At(b.Min.X+x, b.Min.Y+y)
			var nx, ny int
			switch o {
			case 2:
				nx, ny = w-1-x, y
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				nx, ny = x, h-1-y
			case 5:
				nx, ny = y, x
			case 6:
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8:
				nx, ny = y, w-1-x
			}
			dst.Set(nx, ny, c)
		}
	}
	return dst
}

// externalThumb — кадр видео через ffmpeg или первая страница PDF через pdftoppm.
func externalThumb(ctx context.Context, src io.ReadSeeker, srcSize int64, tmpDir string, size int, kind string) ([]byte, error) {
	if srcSize > 4<<30 {
		return nil, ErrUnsupported
	}
	f, err := os.CreateTemp(tmpDir, "thumbsrc-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if kind == "video" {
		vf := fmt.Sprintf("thumbnail,scale=%d:%d:force_original_aspect_ratio=decrease", size, size)
		cmd = exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-ss", "1", "-i", f.Name(),
			"-frames:v", "1", "-vf", vf, "-q:v", "4", "-f", "image2", "-c:v", "mjpeg", "pipe:1")
	} else {
		cmd = exec.CommandContext(ctx, "pdftoppm", "-f", "1", "-l", "1", "-singlefile", "-jpeg",
			"-scale-to", strconv.Itoa(size), f.Name())
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	if (err != nil || out.Len() == 0) && kind == "video" {
		// очень короткое видео: пробуем первый кадр
		cmd = exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-i", f.Name(), "-frames:v", "1",
			"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", size, size),
			"-q:v", "4", "-f", "image2", "-c:v", "mjpeg", "pipe:1")
		out.Reset()
		stderr.Reset()
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err = cmd.Run()
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %v %s", kind, err, strings.TrimSpace(stderr.String()))
	}
	if out.Len() == 0 {
		return nil, ErrUnsupported
	}
	return out.Bytes(), nil
}
