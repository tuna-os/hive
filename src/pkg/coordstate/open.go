package coordstate

import (
	"fmt"
	"strings"
)

// Open parses the store spec a binary accepts. Only "" and "memory" are
// valid, and both return (nil, nil): there is no durable store, and the
// caller keeps state in its own process.
//
// The durable drivers are deliberately not selectable here, per the
// maintainer decision on hivecommons/hive#18464:
//   - ContentsStore ("github", "github:..."): every checkpoint is a commit,
//     which is a GitHub write, and GitHub writes stay with the Hive App.
//   - D1Store ("d1:..."): serverless stores are deferred until phase 1
//     shows advisory quality.
//
// Each is refused with its reason. Embedders that need a driver construct
// it explicitly.
func Open(spec string) (Store, error) {
	switch {
	case spec == "" || spec == "memory":
		return nil, nil
	case spec == "github" || strings.HasPrefix(spec, "github:"):
		return nil, fmt.Errorf("coordstate: %q is a GitHub write (a commit per checkpoint) and is not selectable; GitHub writes stay with the Hive App (hivecommons/hive#18464). Use memory", spec)
	case strings.HasPrefix(spec, "d1:"):
		return nil, fmt.Errorf("coordstate: %q: serverless stores are deferred per hivecommons/hive#18464 and are not selectable. Use memory", spec)
	default:
		return nil, fmt.Errorf("coordstate: unknown store %q (only memory is available)", spec)
	}
}
