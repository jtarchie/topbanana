package server

import (
	"fmt"
	"math/rand/v2"
)

var adjectives = []string{
	"amber", "azure", "bold", "brave", "calm", "cool", "crisp", "dark",
	"dawn", "deep", "dim", "dusk", "fast", "firm", "fleet", "gold",
	"gray", "green", "jade", "keen", "lush", "mild", "mint", "mist",
	"navy", "oak", "pine", "pink", "pure", "quiet", "red", "rose",
	"sage", "salt", "sharp", "silver", "slate", "soft", "stark", "steel",
	"stone", "swift", "teal", "warm", "wild", "wise", "wry", "zeal",
}

var nouns = []string{
	"ant", "arc", "ash", "banana", "bay", "bee", "bird", "brook",
	"buck", "bull", "cat", "cave", "cliff", "cloud", "crab", "crane",
	"creek", "crow", "deer", "dove", "duck", "dune", "eagle", "elk",
	"fern", "field", "finch", "fish", "flame", "flock", "flux", "fog",
	"ford", "fox", "frog", "gale", "gate", "gull", "hawk", "hill",
	"hive", "horn", "hound", "iris", "isle", "jay", "kite", "lake",
	"lark", "leaf", "lion", "lynx", "mango", "mare", "marsh", "mink", "mole",
	"moon", "moose", "moth", "mule", "nest", "newt", "owl", "palm", "path",
	"peak", "pike", "pine", "pond", "pool", "quail", "rail", "ram",
	"raven", "reed", "reef", "ridge", "river", "robin", "rock", "rook",
	"rune", "rush", "sage", "sand", "shoal", "shore", "shrew", "skua",
	"slug", "snipe", "snow", "sparrow", "spring", "stag", "starling", "stem",
	"stone", "storm", "stream", "swift", "thorn", "tide", "toad", "trail",
	"trout", "vale", "vine", "vole", "wave", "weasel", "wolf", "wren", "yak",
}

func newSlug() string {
	adj := adjectives[rand.IntN(len(adjectives))]
	noun := nouns[rand.IntN(len(nouns))]
	num := rand.IntN(100)
	return fmt.Sprintf("%s-%s-%d", adj, noun, num)
}
