package market

import (
	_ "embed"
	"errors"
	"strings"
	"sync"
)

// SEC725-1: the rule for a new password, on top of validPassword's length: not a listed common password, not a common
// word padded with digits or punctuation (SEC725-7), not a simple pattern, and not containing the account's handle. It applies wherever a new password is set (/setup, /register,
// /account/password, -bootstrap-admin, -reset-admin-password), never at sign-in, so an account that already has such a
// password still signs in. It looks nothing up over a network. It is a floor, not a strength meter: it stops the guesses
// a script tries first. scripts/install.sh's admin_password_weak applies the same rule before an install starts (PM-7): a
// change here changes it, and TestInstallerPasswordRuleMatches fails until it does.

// commonPasswordList is the embedded list; its source and licence are in the file's header.
//
//go:embed common_passwords.txt
var commonPasswordList string

var commonPasswords = sync.OnceValue(func() map[string]bool { return readPasswordList(commonPasswordList) })

// readPasswordList reads an embedded list: one entry per line, # starting a comment line.
func readPasswordList(text string) map[string]bool {
	list := make(map[string]bool)
	for line := range strings.Lines(text) {
		if line = strings.TrimRight(line, "\r\n"); line != "" && !strings.HasPrefix(line, "#") {
			list[line] = true
		}
	}
	return list
}

// commonWordList is the embedded list of words padded passwords are built on; its source is in the file's header.
//
//go:embed common_words.txt
var commonWordList string

var commonWords = sync.OnceValue(func() map[string]bool { return readPasswordList(commonWordList) })

// passwordLeet undoes basic leet; "1" reads as either letter, so a base is tried with each.
var passwordLeet = [...]*strings.Replacer{
	strings.NewReplacer("@", "a", "0", "o", "1", "l", "3", "e", "$", "s"),
	strings.NewReplacer("@", "a", "0", "o", "1", "i", "3", "e", "$", "s"),
}

// passwordIsPaddedWord reports a lowercased password that is a common word once the digits and punctuation around it
// are stripped and basic leet is undone (SEC725-7). Only the whole base counts, so a passphrase that holds such a word
// among others passes.
func passwordIsPaddedWord(p string) bool {
	base := strings.TrimFunc(p, func(r rune) bool { return r < 0x80 && (r < 'a' || r > 'z') })
	// Stripped before leet is undone, so padding digits are not read as letters.
	for _, leet := range passwordLeet {
		if commonWords()[leet.Replace(base)] {
			return true
		}
	}
	return false
}

// passwordRuns are the sequences a person walks along when typing a pattern; the digit row also wraps from 0 to 1.
var passwordRuns = [...]string{"1234567890", "qwertyuiop", "asdfghjkl", "zxcvbnm", "abcdefghijklmnopqrstuvwxyz"}

// passwordRunBreaks is how many adjacent pairs may leave a run before a password stops being a pattern: "asdf1234qwer"
// has two. At 12 bytes or more, ordinary text has far more.
const passwordRunBreaks = 2

func passwordRunStep(a, b byte) bool {
	if a == b || a == '0' && b == '1' || a == '1' && b == '0' {
		return true
	}
	for _, run := range passwordRuns {
		i, j := strings.IndexByte(run, a), strings.IndexByte(run, b)
		if i >= 0 && j >= 0 && (i-j == 1 || j-i == 1) {
			return true
		}
	}
	return false
}

// passwordIsRun reports a lowercased password that mostly walks along passwordRuns.
func passwordIsRun(p string) bool {
	breaks := 0
	for i := 1; i < len(p); i++ {
		if !passwordRunStep(p[i-1], p[i]) {
			breaks++
		}
	}
	return breaks <= passwordRunBreaks
}

// passwordRepeats reports a password that is one to four bytes repeated: one character, or "abcxabcxabcx".
func passwordRepeats(p string) bool {
	for unit := 1; unit <= 4; unit++ {
		repeats := true
		for i := unit; i < len(p); i++ {
			if p[i] != p[i%unit] {
				repeats = false
				break
			}
		}
		if repeats {
			return true
		}
	}
	return false
}

// weakPassword reports a new password that a guesser tries first. Case is ignored.
func weakPassword(handle, password string) bool {
	p := strings.ToLower(password)
	return commonPasswords()[p] || passwordIsPaddedWord(p) || passwordRepeats(p) || passwordIsRun(p) ||
		handle != "" && strings.Contains(p, strings.ToLower(handle))
}

// passwordAdvice is what every refusal of a weak new password tells the person to do.
const passwordAdvice = "Use a passphrase of four or more unrelated words."

const passwordWeakReason = "too easy to guess: it is a common password, a common word with digits or symbols added, or a simple pattern, or it contains the handle. " + passwordAdvice

// errWeakPassword refuses a weak new password on a form; errWeakPasswordHost is the host commands' form of it. Neither
// says which rule matched or repeats the password.
var (
	errWeakPassword     = fail(400, "That password is "+passwordWeakReason)
	errWeakPasswordHost = errors.New("that password is " + passwordWeakReason + " Nothing was changed.")
)
