package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const testKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestEncryptedRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	local, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := NewEncrypted(local, testKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 1, segPlain - 1, segPlain, segPlain + 1, 3*segPlain + 777} {
		data := make([]byte, size)
		rand.Read(data)
		key := "k" + filepath.Base(t.Name()) + string(rune('a'+size%26)) + "0000"
		key = key[:6] + hexSize(size)
		if err := enc.Put(ctx, key, bytes.NewReader(data), int64(size)); err != nil {
			t.Fatalf("put %d: %v", size, err)
		}
		// на диске — шифротекст правильного размера и без открытого текста
		raw, _ := os.ReadFile(filepath.Join(dir, key[:2], key[2:4], key))
		if int64(len(raw)) != CipherSize(int64(size)) {
			t.Fatalf("size %d: cipher %d, want %d", size, len(raw), CipherSize(int64(size)))
		}
		if size > 32 && bytes.Contains(raw, data[:32]) {
			t.Fatalf("plaintext leaked")
		}
		b, err := enc.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if b.Size() != int64(size) {
			t.Fatalf("plain size %d, want %d", b.Size(), size)
		}
		got, _ := io.ReadAll(b)
		if !bytes.Equal(got, data) {
			t.Fatalf("size %d: data mismatch", size)
		}
		// произвольный Seek внутри и через границы сегментов
		if size > 10 {
			off := int64(size) - 5
			b.Seek(off, io.SeekStart)
			tail, _ := io.ReadAll(b)
			if !bytes.Equal(tail, data[off:]) {
				t.Fatalf("seek tail mismatch")
			}
			b.Seek(int64(size/2), io.SeekStart)
			buf := make([]byte, 3)
			io.ReadFull(b, buf)
			if !bytes.Equal(buf, data[size/2:size/2+3]) {
				t.Fatalf("seek middle mismatch")
			}
		}
		b.Close()
	}
}

func hexSize(n int) string {
	const h = "0123456789abcdef"
	s := []byte("0000000000")
	for i := len(s) - 1; i >= 0 && n > 0; i-- {
		s[i] = h[n%16]
		n /= 16
	}
	return string(s)
}

func TestEncryptedTamperAndLegacy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	local, _ := NewLocal(dir)
	enc, _ := NewEncrypted(local, testKey)
	data := bytes.Repeat([]byte("секрет "), 30000)
	enc.Put(ctx, "aabbccdd01", bytes.NewReader(data), int64(len(data)))

	// подмена байта — расшифровка должна упасть
	p := filepath.Join(dir, "aa", "bb", "aabbccdd01")
	raw, _ := os.ReadFile(p)
	raw[100] ^= 1
	os.WriteFile(p, raw, 0o600)
	// Блобы обязательно закрываем: на Windows открытый файл не даёт t.TempDir() удалить каталог.
	get := func(s *Encrypted, key string) Blob {
		t.Helper()
		b, err := s.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close() })
		return b
	}

	if _, err := io.ReadAll(get(enc, "aabbccdd01")); err == nil {
		t.Fatal("tampered blob decrypted without error")
	}

	// обрезка последнего сегмента — тоже ошибка (флаг последнего сегмента)
	enc.Put(ctx, "aabbccdd02", bytes.NewReader(data), int64(len(data)))
	p2 := filepath.Join(dir, "aa", "bb", "aabbccdd02")
	raw, _ = os.ReadFile(p2)
	os.WriteFile(p2, raw[:headerSize+segCipher], 0o600) // оставили только первый сегмент
	if _, err := io.ReadAll(get(enc, "aabbccdd02")); err == nil {
		t.Fatal("truncated blob decrypted without error")
	}

	// чужой ключ
	other, _ := NewEncrypted(local, "ff"+testKey[2:])
	enc.Put(ctx, "aabbccdd03", bytes.NewReader(data), int64(len(data)))
	if _, err := io.ReadAll(get(other, "aabbccdd03")); err == nil {
		t.Fatal("wrong key decrypted")
	}

	// старый незашифрованный блоб читается как есть
	local.Put(ctx, "aabbccdd04", bytes.NewReader([]byte("plain old data")), 14)
	got, _ := io.ReadAll(get(enc, "aabbccdd04"))
	if string(got) != "plain old data" {
		t.Fatalf("legacy blob: %q", got)
	}
}
