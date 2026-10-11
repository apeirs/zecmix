package lwd

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// DisplayHash reverses a little-endian hash (as lightwalletd sends block and
// tx hashes) into the usual big-endian hex shown by explorers.
func DisplayHash(b []byte) string {
	r := make([]byte, len(b))
	for i := range b {
		r[len(b)-1-i] = b[i]
	}
	return hex.EncodeToString(r)
}

// ParseDisplayHash turns explorer-style hex into lightwalletd's byte order.
func ParseDisplayHash(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("want a 64-hex-char txid, got %q", s)
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b, nil
}

// TxHeader is what we can read from a raw tx without a full parser.
type TxHeader struct {
	Version        uint32
	Overwintered   bool
	VersionGroupID uint32
	ConsensusBrID  uint32 // v5 only
	Size           int
}

func (h TxHeader) String() string {
	s := fmt.Sprintf("v%d", h.Version)
	if h.Overwintered {
		s += fmt.Sprintf(" (overwintered, version group %08x", h.VersionGroupID)
		if h.Version >= 5 {
			s += fmt.Sprintf(", consensus branch %08x", h.ConsensusBrID)
		}
		s += ")"
	}
	return fmt.Sprintf("%s, %d bytes", s, h.Size)
}

// Known version group ids.
var versionGroups = map[uint32]uint32{3: 0x03C48270, 4: 0x892F2085, 5: 0x26A7270A}

// DecodeRawTx decodes hex (whitespace allowed) and sanity-checks the header,
// catching copy/paste mistakes before anything is sent.
func DecodeRawTx(s string) ([]byte, TxHeader, error) {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, TxHeader{}, fmt.Errorf("transaction is not valid hex: %w", err)
	}
	if len(raw) < 10 {
		return nil, TxHeader{}, errors.New("transaction too short")
	}
	hdr := binary.LittleEndian.Uint32(raw[0:4])
	h := TxHeader{Version: hdr & 0x7fffffff, Overwintered: hdr&0x80000000 != 0, Size: len(raw)}
	if h.Overwintered {
		h.VersionGroupID = binary.LittleEndian.Uint32(raw[4:8])
		if want, ok := versionGroups[h.Version]; ok && want != h.VersionGroupID {
			return nil, h, fmt.Errorf("v%d tx with unexpected version group id %08x (want %08x)", h.Version, h.VersionGroupID, want)
		}
		if h.Version >= 5 {
			h.ConsensusBrID = binary.LittleEndian.Uint32(raw[8:12])
		}
	} else if h.Version < 1 || h.Version > 2 {
		return nil, h, fmt.Errorf("unrecognised transaction header %08x", hdr)
	}
	return raw, h, nil
}
