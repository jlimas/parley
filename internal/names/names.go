// Package names generates short client identifiers and random display names
// for new API keys. The ID is a 6-character base-36 string (≈2.1B values);
// collision probability for 100 clients is < 0.003%. Display names are
// single memorable words drawn from a fixed dictionary.
package names

import (
	"crypto/rand"
	"encoding/binary"
)

// Words is the display-name dictionary. Pick() selects uniformly at random.
var Words = []string{
	// animals
	"albatross", "badger", "bear", "beaver", "bison", "bobcat", "condor",
	"cormorant", "cougar", "crane", "crow", "dingo", "eagle", "egret",
	"falcon", "ferret", "finch", "fox", "gecko", "heron", "ibis",
	"jackal", "jaguar", "kestrel", "kite", "lynx", "marten", "merlin",
	"mink", "mongoose", "moose", "narwhal", "newt", "orca", "osprey",
	"otter", "panda", "panther", "parrot", "pelican", "peregrine", "plover",
	"puffin", "quail", "raven", "robin", "salamander", "sandpiper", "skunk",
	"sloth", "sparrow", "stork", "swift", "tanager", "tapir", "teal",
	"thrush", "toucan", "viper", "vole", "weasel", "wolf", "wolverine",
	"wren", "yak",
	// nature / landscape
	"amber", "aspen", "basalt", "birch", "canyon", "cedar", "cliff",
	"cobalt", "coral", "dune", "ember", "fjord", "flint", "frost",
	"gale", "garnet", "glacier", "granite", "grove", "gust", "hazel",
	"heath", "inlet", "jade", "jasper", "kelp", "larch", "ledge",
	"linden", "maple", "marsh", "mesa", "mist", "mossy", "oak",
	"obsidian", "onyx", "opal", "pebble", "pine", "prism", "quartz",
	"reed", "ridge", "rime", "ripple", "rowan", "russet", "sage",
	"schist", "shale", "slate", "sorrel", "spruce", "stone", "thorn",
	"tide", "timber", "tundra", "vale", "walnut", "willow", "yarrow",
	// space / celestial
	"apex", "arc", "aurora", "beacon", "bolt", "comet", "corona",
	"crest", "cygnus", "dawn", "dusk", "echo", "flare", "forge",
	"helm", "kindle", "loft", "nebula", "nexus", "nova", "orbit",
	"photon", "pulsar", "pulse", "quasar", "reef", "roam", "shade",
	"signal", "solstice", "surge", "terra", "vault", "veil", "wave",
	"zenith", "zephyr",
}

const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

// GenClientID returns a random 6-character base-36 string.
// The alphabet is digits + lowercase letters; values span 36^6 ≈ 2.17 billion.
func GenClientID() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("names: crypto/rand unavailable: " + err.Error())
	}
	n := binary.BigEndian.Uint32(buf[:]) % 2_176_782_336 // 36^6
	out := make([]byte, 6)
	for i := 5; i >= 0; i-- {
		out[i] = base36[n%36]
		n /= 36
	}
	return string(out)
}

// Pick returns a random word from Words.
func Pick() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("names: crypto/rand unavailable: " + err.Error())
	}
	n := binary.BigEndian.Uint32(buf[:])
	return Words[int(n)%len(Words)]
}
