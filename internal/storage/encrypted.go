package storage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Encrypted — обёртка, шифрующая блобы AES-256-GCM.
//
// Формат: "CBENC001" + сегменты. Каждый сегмент — до 64 КиБ открытого текста + 16 байт тега.
// Ключ блоба = HKDF-SHA256(мастер-ключ, info = ключ блоба), поэтому nonce может быть счётчиком:
// nonce = [флаг последнего сегмента (1 байт)] [3 нуля] [номер сегмента (8 байт)].
// Флаг последнего сегмента защищает от обрезки файла. Сегменты позволяют Seek без расшифровки всего файла.
// Блобы без заголовка (записанные до включения шифрования) читаются как есть.
type Encrypted struct {
	Inner Storage
	key   []byte
}

const (
	encMagic   = "CBENC001"
	segPlain   = 64 << 10
	segOverhd  = 16
	segCipher  = segPlain + segOverhd
	headerSize = len(encMagic)
)

func NewEncrypted(inner Storage, hexKey string) (*Encrypted, error) {
	k, err := hex.DecodeString(hexKey)
	if err != nil || len(k) != 32 {
		return nil, errors.New("CB_ENCRYPTION_KEY: нужно 64 шестнадцатеричных символа (openssl rand -hex 32)")
	}
	return &Encrypted{Inner: inner, key: k}, nil
}

func (e *Encrypted) aead(blobKey string) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha256.New, e.key, nil, "cloudbox-blob:"+blobKey, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(idx uint64, last bool) []byte {
	n := make([]byte, 12)
	if last {
		n[0] = 1
	}
	binary.BigEndian.PutUint64(n[4:], idx)
	return n
}

func segments(plain int64) int64 {
	if plain == 0 {
		return 1
	}
	return (plain + segPlain - 1) / segPlain
}

// CipherSize — размер зашифрованного блоба для открытого текста размера plain.
func CipherSize(plain int64) int64 { return int64(headerSize) + plain + segments(plain)*segOverhd }

func plainSize(c int64) (int64, error) {
	body := c - int64(headerSize)
	if body < segOverhd {
		return 0, errors.New("encrypted blob too short")
	}
	full := body / segCipher
	rem := body % segCipher
	if rem == 0 {
		return full * segPlain, nil
	}
	if rem < segOverhd {
		return 0, errors.New("corrupt encrypted blob")
	}
	return full*segPlain + rem - segOverhd, nil
}

func (e *Encrypted) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	a, err := e.aead(key)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := pw.Write([]byte(encMagic))
		buf := make([]byte, segPlain)
		var idx uint64
		var done int64
		for err == nil {
			n, rerr := io.ReadFull(r, buf)
			if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
				err = rerr
				break
			}
			done += int64(n)
			last := done >= size
			if n == 0 && !last {
				err = io.ErrUnexpectedEOF
				break
			}
			ct := a.Seal(nil, nonce(idx, last), buf[:n], nil)
			if _, werr := pw.Write(ct); werr != nil {
				err = werr
				break
			}
			idx++
			if last {
				break
			}
		}
		pw.CloseWithError(err)
	}()
	err = e.Inner.Put(ctx, key, pr, CipherSize(size))
	pr.CloseWithError(err)
	return err
}

func (e *Encrypted) Delete(ctx context.Context, key string) error { return e.Inner.Delete(ctx, key) }

func (e *Encrypted) Get(ctx context.Context, key string) (Blob, error) {
	b, err := e.Inner.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, headerSize)
	n, _ := io.ReadFull(b, hdr)
	if n < headerSize || string(hdr) != encMagic {
		// незашифрованный блоб из времён до включения шифрования
		if _, err := b.Seek(0, io.SeekStart); err != nil {
			b.Close()
			return nil, err
		}
		return b, nil
	}
	ps, err := plainSize(b.Size())
	if err != nil {
		b.Close()
		return nil, err
	}
	a, err := e.aead(key)
	if err != nil {
		b.Close()
		return nil, err
	}
	return &encBlob{inner: b, aead: a, size: ps, segs: segments(ps), cur: -1}, nil
}

type encBlob struct {
	inner Blob
	aead  cipher.AEAD
	size  int64
	segs  int64
	pos   int64
	cur   int64  // номер расшифрованного сегмента в plain
	plain []byte // открытый текст текущего сегмента
	ct    []byte
}

func (b *encBlob) Size() int64  { return b.size }
func (b *encBlob) Close() error { return b.inner.Close() }

func (b *encBlob) load(idx int64) error {
	if idx == b.cur {
		return nil
	}
	off := int64(headerSize) + idx*segCipher
	if _, err := b.inner.Seek(off, io.SeekStart); err != nil {
		return err
	}
	want := segCipher
	if idx == b.segs-1 {
		want = int(b.size-idx*segPlain) + segOverhd
	}
	if cap(b.ct) < want {
		b.ct = make([]byte, want)
	}
	ct := b.ct[:want]
	if _, err := io.ReadFull(b.inner, ct); err != nil {
		return fmt.Errorf("encrypted read: %w", err)
	}
	pt, err := b.aead.Open(b.plain[:0], nonce(uint64(idx), idx == b.segs-1), ct, nil)
	if err != nil {
		return errors.New("blob decryption failed (wrong key or corrupted data)")
	}
	b.plain, b.cur = pt, idx
	return nil
}

func (b *encBlob) Read(p []byte) (int, error) {
	if b.pos >= b.size {
		return 0, io.EOF
	}
	idx := b.pos / segPlain
	if err := b.load(idx); err != nil {
		return 0, err
	}
	n := copy(p, b.plain[b.pos-idx*segPlain:])
	b.pos += int64(n)
	return n, nil
}

func (b *encBlob) Seek(off int64, whence int) (int64, error) {
	var np int64
	switch whence {
	case io.SeekStart:
		np = off
	case io.SeekCurrent:
		np = b.pos + off
	case io.SeekEnd:
		np = b.size + off
	}
	if np < 0 {
		return 0, errors.New("negative position")
	}
	b.pos = np
	return np, nil
}

// ReadAt нужен для zip-форматов при индексации.
func (b *encBlob) ReadAt(p []byte, off int64) (int, error) {
	save := b.pos
	defer func() { b.pos = save }()
	b.pos = off
	total := 0
	for total < len(p) {
		n, err := b.Read(p[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
