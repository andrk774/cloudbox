//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// protect шифрует данные DPAPI: расшифровать их может только этот пользователь Windows на этом компьютере.
func protect(data []byte) ([]byte, error) { return dpapi(data, true) }

func unprotect(data []byte) ([]byte, error) { return dpapi(data, false) }

func dpapi(data []byte, enc bool) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	const uiForbidden = 0x1 // CRYPTPROTECT_UI_FORBIDDEN
	var err error
	if enc {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, uiForbidden, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, uiForbidden, &out)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
