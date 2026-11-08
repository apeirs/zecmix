//go:build !unix

package mixnet

import (
	"errors"
	"os"
)

func socketpair() (ours, theirs *os.File, err error) {
	return nil, nil, errors.New("the nymdial transport needs a Unix-like OS (socketpair); use --transport socks5")
}
