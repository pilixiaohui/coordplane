package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

func main() {
	mode := "full"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	text, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	switch mode {
	case "tokenize":
		for _, token := range Tokenize(string(text)) {
			fmt.Println(token)
		}
	case "count":
		// whitespace split only, so the count module is independently
		// verifiable without the tokenize module
		printCounts(Count(strings.Fields(string(text))))
	case "full":
		printCounts(Count(Tokenize(string(text))))
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", mode)
		os.Exit(2)
	}
}

func printCounts(counts map[string]int) {
	words := make([]string, 0, len(counts))
	for word := range counts {
		words = append(words, word)
	}
	sort.Strings(words)
	for _, word := range words {
		fmt.Printf("%s %d\n", word, counts[word])
	}
}
