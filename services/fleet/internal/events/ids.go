package events

import (
	"regexp"
	"strings"

	"github.com/oklog/ulid/v2"
)

// ID prefixes (contracts/README.md, "IDs"). An ID is "<prefix>_<ULID>", for example
// "cou_01JA2B3C4D5E6F7G8H9JKMNPQR".
const (
	PrefixEvent    = "evt"
	PrefixOrder    = "ord"
	PrefixMerchant = "mer"
	PrefixStore    = "str"
	PrefixCourier  = "cou"
	PrefixOffer    = "off"
)

// idPattern is the schemas' pattern without the prefix: 26 upper-case Crockford base32 characters.
var idPattern = regexp.MustCompile(`^[a-z]{3}_[0-9A-HJKMNP-TV-Z]{26}$`)

// NewID returns a new prefixed ULID. ULIDs sort by creation time and are not guessable, and
// ulid.Make is safe for concurrent use.
func NewID(prefix string) string {
	return prefix + "_" + ulid.Make().String()
}

// ValidID reports whether id has the given prefix and the exact shape the schemas require.
func ValidID(prefix, id string) bool {
	return strings.HasPrefix(id, prefix+"_") && idPattern.MatchString(id)
}
