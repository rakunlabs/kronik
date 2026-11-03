package executor

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// placeholder matches {{secret.NAME}} and {{param.NAME}}.
var placeholder = regexp.MustCompile(`\{\{\s*(secret|param)\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// expand fills in {{secret.NAME}} and {{param.NAME}}. A secret that is not
// there is an error; a param that was not given is empty.
func expand(s string, info Info) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}

	var missing []string

	out := placeholder.ReplaceAllStringFunc(s, func(m string) string {
		p := placeholder.FindStringSubmatch(m)
		if p[1] == "param" {
			return info.Params[p[2]]
		}

		v, ok := info.Secrets[p[2]]
		if !ok {
			missing = append(missing, p[2])
		}

		return v
	})

	if len(missing) > 0 {
		slices.Sort(missing)

		return "", fmt.Errorf("unknown secret %s (Settings → Secrets)", strings.Join(slices.Compact(missing), ", "))
	}

	return out, nil
}

// expandMap expands every value of m.
func expandMap(m map[string]string, info Info) (map[string]string, error) {
	out := make(map[string]string, len(m))

	for k, v := range m {
		x, err := expand(v, info)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}

		out[k] = x
	}

	return out, nil
}

// paramEnv is the run's params as KRONIK_PARAM_<NAME> variables.
func paramEnv(info Info) map[string]string {
	out := make(map[string]string, len(info.Params))
	for k, v := range info.Params {
		out["KRONIK_PARAM_"+strings.ToUpper(k)] = v
	}

	return out
}

// ParamName is the form of a param's name.
var ParamName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

const (
	maxParams     = 50
	maxParamValue = 8 << 10
)

// CheckParams checks the params given to a run.
func CheckParams(p map[string]string) error {
	if len(p) > maxParams {
		return fmt.Errorf("at most %d params", maxParams)
	}

	for _, k := range slices.Sorted(maps.Keys(p)) {
		if !ParamName.MatchString(k) {
			return fmt.Errorf("param name %q: letters, digits and _, not starting with a digit", k)
		}

		if len(p[k]) > maxParamValue {
			return fmt.Errorf("param %s is longer than %d KiB", k, maxParamValue>>10)
		}
	}

	return nil
}

// SecretValues are the values to hide in a run's output.
func SecretValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}

	return out
}
