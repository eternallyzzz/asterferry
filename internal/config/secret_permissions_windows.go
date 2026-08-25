//go:build windows

package config

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ValidateSecretFilePermissions(path string, info os.FileInfo) error {
	if info == nil {
		return errors.New("secret file metadata is missing")
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("inspect secret file ACL: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		if err == nil {
			err = errors.New("file owner is missing")
		}
		return fmt.Errorf("inspect secret file owner: %w", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		if err == nil {
			err = errors.New("file DACL is missing")
		}
		return fmt.Errorf("inspect secret file DACL: %w", err)
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return fmt.Errorf("inspect secret file ACE: %w", err)
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		// Windows user profiles commonly inherit the parent directory ACL. The
		// parent directory is the boundary that controls whether another user
		// can reach the file; only explicit file-level grants are rejected here.
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if owner.Equals(aceSID) {
			continue
		}
		mask := uint32(ace.Mask)
		writeMask := uint32(windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA | windows.DELETE)
		if mask&writeMask != 0 {
			return errors.New("secret file grants write access to a non-owner principal")
		}
	}
	return nil
}
