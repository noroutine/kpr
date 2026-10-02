package proof

import (
	"context"
	"fmt"
	"net/http"
)

// RegistryReadonly is a classified registry that refuses writes: the
// probe answered 405. Sealed like every evidence: the only
// inhabitant comes from ProveMode, and the zero value is nil.
type RegistryReadonly interface {
	sealed()
}

type registryReadonly struct{}

func (registryReadonly) sealed() {}

// RegistryWritable is a classified registry that takes writes: the
// probe answered 202. Sealed like every evidence: the only
// inhabitant comes from ProveMode, and the zero value is nil.
type RegistryWritable interface {
	sealed()
}

type registryWritable struct{}

func (registryWritable) sealed() {}

// Initiator performs one upload-initiate round trip against the
// peer: POST, best-effort DELETE of our own upload on 202, and
// report the status. The round trip leaves no blob, no manifest,
// no residue — that duty stays with the transport, beside the
// HTTP. The registry peer satisfies it; tests script it.
type Initiator interface {
	Initiate(ctx context.Context, baseURL string) (status int, err error)
}

// ProveMode mints exactly one: 202 proves writable, 405 proves
// readonly, anything else — and any transport failure — mints
// nothing and returns the cause. Inconclusive is an error, so a
// caller that ignores it collects blind; a caller that wants the
// classification takes whichever token came back non-nil.
//
// Still courtesy-grade: this classifies the registry, never our
// store. Sealing changes the plumbing (the probe cannot be
// skipped or forged), not what is proven.
func ProveMode(ctx context.Context, in Initiator, baseURL string) (RegistryReadonly, RegistryWritable, error) {
	st, err := in.Initiate(ctx, baseURL)
	if err != nil {
		return nil, nil, err
	}
	switch st {
	case http.StatusAccepted:
		return nil, registryWritable{}, nil
	case http.StatusMethodNotAllowed:
		return registryReadonly{}, nil, nil
	default:
		return nil, nil, fmt.Errorf("sentinel inconclusive: status %d from %s, want 202 (writable) or 405 (readonly)", st, baseURL)
	}
}
