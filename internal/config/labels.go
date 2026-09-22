package config

import (
	"fmt"
	"maps"
	"regexp"
	"strings"
)

const (
	LabelManagedBy   = "managed-by"
	ManagedByValue   = "hinfect"
	LabelFlake       = "hinfect-flake"
	LabelStatus      = "hinfect-status"
	StatusInstalling = "installing"
	StatusReady      = "ready"
)

var (
	labelNameRe       = regexp.MustCompile(`^[a-zA-Z0-9]([-a-zA-Z0-9_.]{0,61}[a-zA-Z0-9])?$`)
	labelPrefixRe     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	invalidValueChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)
)

// ValidLabelKey reports whether k is a valid Hetzner label key: a name,
// optionally preceded by a DNS-subdomain prefix and "/".
func ValidLabelKey(k string) bool {
	prefix, name, hasPrefix := strings.Cut(k, "/")
	if !hasPrefix {
		return labelNameRe.MatchString(k)
	}
	return len(prefix) <= 253 && labelPrefixRe.MatchString(prefix) && labelNameRe.MatchString(name)
}

func ValidLabelValue(v string) bool { return v == "" || labelNameRe.MatchString(v) }

// SanitizeLabelValue turns an arbitrary string into a valid label value.
func SanitizeLabelValue(s string) string {
	s = invalidValueChars.ReplaceAllString(s, "-")
	s = strings.TrimFunc(s, notAlnum)
	if len(s) > 63 {
		s = strings.TrimFunc(s[:63], notAlnum)
	}
	return s
}

func notAlnum(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
}

// ParseLabels parses repeated --label key=value flags.
func ParseLabels(pairs []string) (map[string]string, error) {
	labels := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		switch {
		case !ok:
			return nil, fmt.Errorf("label %q must have the form key=value", p)
		case !ValidLabelKey(k):
			return nil, fmt.Errorf("invalid label key %q", k)
		case !ValidLabelValue(v):
			return nil, fmt.Errorf("invalid label value %q for key %q", v, k)
		case k == LabelManagedBy || strings.HasPrefix(k, "hinfect-"):
			return nil, fmt.Errorf("label key %q is reserved for hinfect", k)
		}
		labels[k] = v
	}
	return labels, nil
}

// SystemLabels returns a copy of user with the labels hinfect always sets.
func SystemLabels(user map[string]string, flake FlakeRef, status string) map[string]string {
	labels := maps.Clone(user)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelManagedBy] = ManagedByValue
	labels[LabelFlake] = SanitizeLabelValue(flake.String())
	labels[LabelStatus] = status
	return labels
}
