package govarnam

import (
	sql "database/sql"
	"fmt"
	"os"
	"path"
	"sort"
	"unicode/utf8"

	// sqlite3
	_ "github.com/mattn/go-sqlite3"
)

type LangRules struct {
	Virama      string
	IndicDigits bool
}

// Varnam config
type Varnam struct {
	vstConn   *sql.DB
	dictConn  *sql.DB
	LangRules LangRules
	debug     bool
}

// Suggestion suggestion
type Suggestion struct {
	Word      string
	Weight    int
	LearnedOn int
}

// TransliterationResult result
type TransliterationResult struct {
	ExactMatch      []Suggestion
	Suggestions     []Suggestion
	GreedyTokenized []Suggestion
}

func (varnam *Varnam) getNewValueAndWeight(weight int, symbol Symbol, previousCharacter string, tokensLength int, position int) (string, int) {
	/**
	 * Weight priority:
	 * 1. Position of character in string
	 * 2. Symbol's probability occurence
	 */
	newWeight := weight - symbol.weight + (tokensLength - position) + (VARNAM_MATCH_POSSIBILITY - symbol.matchType)

	var value string

	if symbol.generalType == VARNAM_SYMBOL_VIRAMA {
		/*
			we are resolving a virama. If the output ends with a virama already,
			add a ZWNJ to it, so that following character will not be combined.
			If output does not end with virama, add a virama and ZWNJ
		*/
		if previousCharacter == varnam.LangRules.Virama {
			value = ZWNJ
		} else {
			value = getSymbolValue(symbol, position) + ZWNJ
		}
	} else {
		value = getSymbolValue(symbol, position)
	}

	return value, newWeight
}

/**
 * greed - Set to true for getting only VARNAM_MATCH_EXACT suggestions.
 * partial - set true if only a part of a word is being tokenized and not an entire word
 */
func (varnam *Varnam) tokensToSuggestions(tokens []Token, greedy bool, partial bool) []Suggestion {
	var results []Suggestion

	for i, t := range tokens {
		if t.tokenType == VARNAM_TOKEN_SYMBOL {
			var state int
			if i == 0 {
				state = VARNAM_TOKEN_ACCEPT_IF_STARTS_WITH
			} else if i+1 == len(tokens) {
				state = VARNAM_TOKEN_ACCEPT_IF_ENDS_WITH
			} else {
				state = VARNAM_TOKEN_ACCEPT_IF_IN_BETWEEN
			}

			if i == 0 {
				for _, possibility := range t.token {
					if greedy && possibility.matchType == VARNAM_MATCH_POSSIBILITY {
						continue
					}

					if possibility.acceptCondition != VARNAM_TOKEN_ACCEPT_ALL && possibility.acceptCondition != state {
						continue
					}

					var value string
					if partial {
						value = getSymbolValue(possibility, 1)
					} else {
						value = getSymbolValue(possibility, 0)
					}

					sug := Suggestion{value, VARNAM_TOKEN_BASIC_WEIGHT - possibility.weight, 0}
					results = append(results, sug)
				}
			} else {
				for j, result := range results {
					till := result.Word
					tillWeight := result.Weight

					firstToken := t.token[0]

					lastChar, _ := getLastCharacter(till)
					newValue, newWeight := varnam.getNewValueAndWeight(results[j].Weight, firstToken, lastChar, len(tokens), i)

					results[j].Word += newValue
					results[j].Weight = newWeight

					for k, possibility := range t.token {
						if k == 0 || (greedy && possibility.matchType == VARNAM_MATCH_POSSIBILITY) {
							continue
						}

						if possibility.acceptCondition != VARNAM_TOKEN_ACCEPT_ALL && possibility.acceptCondition != state {
							continue
						}

						lastChar, _ := getLastCharacter(till)
						newValue, newWeight := varnam.getNewValueAndWeight(tillWeight, possibility, lastChar, len(tokens), i)

						newTill := till + newValue

						sug := Suggestion{newTill, newWeight, 0}
						results = append(results, sug)
					}
				}
			}
		} else if t.tokenType == VARNAM_TOKEN_CHAR {
			for i := range results {
				results[i].Word += *t.character
			}
		}
	}

	return results
}

func (varnam *Varnam) setLangRules() {
	varnam.LangRules.IndicDigits = false
	varnam.LangRules.Virama = varnam.searchSymbol("~", VARNAM_MATCH_EXACT)[0].value1
}

func getLastCharacter(input string) (string, int) {
	r, size := utf8.DecodeLastRuneInString(input)
	if r == utf8.RuneError && (size == 0 || size == 1) {
		size = 0
	}
	return input[len(input)-size:], size
}

func (varnam *Varnam) removeLastVirama(input string) string {
	char, size := getLastCharacter(input)
	if char == varnam.LangRules.Virama {
		return input[0 : len(input)-size]
	}
	return input
}

func sortSuggestions(sugs []Suggestion) []Suggestion {
	sort.SliceStable(sugs, func(i, j int) bool {
		return sugs[i].LearnedOn > sugs[j].LearnedOn || sugs[i].Weight > sugs[j].Weight
	})
	return sugs
}

// Transliterate a word
func (varnam *Varnam) Transliterate(word string) TransliterationResult {
	var (
		results               []Suggestion
		transliterationResult TransliterationResult
	)

	tokens := varnam.tokenizeWord(word, VARNAM_MATCH_ALL)

	/* Channels make things faster, getting from DB is time-consuming */

	dictSugsChan := make(chan DictionaryResult)
	patternDictSugsChan := make(chan []PatternDictionarySuggestion)
	greedyTokenizedChan := make(chan []Suggestion)

	moreFromDictChan := make(chan [][]Suggestion)
	triggeredGetMoreFromDict := false

	go varnam.channelGetFromDictionary(tokens, dictSugsChan)
	go varnam.channelGetFromPatternDictionary(word, patternDictSugsChan)
	go varnam.channelTokensToSuggestions(tokens, true, false, greedyTokenizedChan)

	dictSugs := <-dictSugsChan

	if varnam.debug {
		fmt.Println("Dictionary results:", dictSugs)
	}

	if len(dictSugs.sugs) > 0 {
		if dictSugs.exactMatch == false {
			// These will be partial words
			restOfWord := word[dictSugs.longestMatchPosition+1:]
			results = varnam.tokenizeRestOfWord(restOfWord, dictSugs.sugs)
		} else {
			transliterationResult.ExactMatch = dictSugs.sugs

			// Since partial words are in dictionary, exactMatch will be TRUE
			// for pathway to a word. Hence we're calling this here
			go varnam.channelGetMoreFromDictionary(dictSugs.sugs, moreFromDictChan)
			triggeredGetMoreFromDict = true
		}
	}

	patternDictSugs := <-patternDictSugsChan

	if len(patternDictSugs) > 0 {
		if varnam.debug {
			fmt.Println("Pattern dictionary results:", patternDictSugs)
		}

		for _, match := range patternDictSugs {
			if match.Length < len(word) {
				restOfWord := word[match.Length:]
				filled := varnam.tokenizeRestOfWord(restOfWord, []Suggestion{match.Sug})
				results = append(results, filled...)
			} else if match.Length == len(word) {
				// Same length
				transliterationResult.ExactMatch = append(transliterationResult.ExactMatch, match.Sug)
			} else {
				results = append(results, match.Sug)
			}
		}
	}

	if triggeredGetMoreFromDict {
		moreFromDict := <-moreFromDictChan

		if varnam.debug {
			fmt.Println("More dictionary results:", moreFromDict)
		}

		for _, sugSet := range moreFromDict {
			for _, sug := range sugSet {
				results = append(results, sug)
			}
		}
	}

	if len(transliterationResult.ExactMatch) == 0 {
		sugs := varnam.tokensToSuggestions(tokens, false, false)
		results = append(results, sugs...)
	}

	transliterationResult.ExactMatch = sortSuggestions(transliterationResult.ExactMatch)
	transliterationResult.Suggestions = sortSuggestions(results)

	// Add greedy tokenized suggestions. This will only give exact match (VARNAM_MATCH_EXACT) results
	transliterationResult.GreedyTokenized = sortSuggestions(<-greedyTokenizedChan)

	return transliterationResult
}

// Init Initialize varnam
func Init(vstPath string, dictPath string) Varnam {
	varnam := Varnam{}
	varnam.openVST(vstPath)
	varnam.openDict(dictPath)
	varnam.setLangRules()
	return varnam
}

// InitFromLang code
func InitFromLang(langCode string) (*Varnam, error) {
	var (
		vstPath  *string = nil
		dictPath string
	)

	vstPath = findVSTPath(langCode)

	dictPath = findLearningsFilePath(langCode)
	if !fileExists(dictPath) {
		fmt.Printf("Making Varnam Learnings File at %s\n", dictPath)
		os.MkdirAll(path.Dir(dictPath), 0750)
		makeDictionary(dictPath)
	}

	if vstPath == nil {
		return nil, fmt.Errorf("Couldn't find VST")
	}

	varnam := Init(*vstPath, dictPath)

	return &varnam, nil
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

func dirExists(loc string) bool {
	info, err := os.Stat(loc)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}

// Debug turn on or off debug messages
func (varnam *Varnam) Debug(val bool) {
	varnam.debug = val
}

// Close close db connections
func (varnam *Varnam) Close() {
	defer varnam.vstConn.Close()
	defer varnam.dictConn.Close()
}
