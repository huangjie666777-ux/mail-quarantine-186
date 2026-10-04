package main

import (
	"errors"
	"regexp"
	"strings"
)

var errInvalidAddress = errors.New("invalid email address")

var dotAtomLocal = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+/=?^_` + "`" + `{|}~-]+(?:\.[A-Za-z0-9!#$%&'*+/=?^_` + "`" + `{|}~-]+)*$`)
var domainLabel = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func CanonicalRecipient(address string) (string, error) {
	at := strings.LastIndexByte(address, '@')
	if at <= 0 || at != strings.IndexByte(address, '@') {
		return "", errInvalidAddress
	}
	local := address[:at]
	domain := address[at+1:]
	if !dotAtomLocal.MatchString(local) {
		return "", errInvalidAddress
	}
	if !validDomain(domain) {
		return "", errInvalidAddress
	}
	return local + "@" + strings.ToLower(domain), nil
}

func validDomain(domain string) bool {
	if len(domain) == 0 || len(domain) > 253 || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if !domainLabel.MatchString(label) {
			return false
		}
	}
	return true
}
