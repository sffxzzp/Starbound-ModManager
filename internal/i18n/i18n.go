package i18n

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

type Locale string
type Key string

const (
	Chinese Locale = "zh-CN"
	English Locale = "en-US"
)

//go:embed locales/*.json
var localeFiles embed.FS

var catalogs = loadCatalogs()

type Message struct {
	Key  Key
	Args []any
}

func Msg(key Key, args ...any) Message {
	return Message{Key: key, Args: args}
}

type LocalizedError struct {
	Message       Message
	Cause         error
	causeArgument bool
}

func NewError(key Key, args ...any) error {
	return &LocalizedError{Message: Msg(key, args...)}
}

func WrapError(key Key, cause error, args ...any) error {
	return &LocalizedError{Message: Msg(key, args...), Cause: cause, causeArgument: true}
}

// WrapErrorFormat keeps cause in the error chain without adding it to the
// formatted arguments. Use it when the message arguments already contain
// localized representations of one or more errors.
func WrapErrorFormat(key Key, cause error, args ...any) error {
	return &LocalizedError{Message: Msg(key, args...), Cause: cause}
}

func (e *LocalizedError) Error() string {
	return ErrorText(Chinese, e)
}

func (e *LocalizedError) Unwrap() error {
	return e.Cause
}

func IsSupported(locale string) bool {
	return Locale(locale) == Chinese || Locale(locale) == English
}

func Normalize(locale string) Locale {
	if IsSupported(locale) {
		return Locale(locale)
	}
	return Chinese
}

func Translate(locale Locale, key Key, args ...any) string {
	locale = Normalize(string(locale))
	template, ok := catalogs[locale][key]
	if !ok {
		template, ok = catalogs[English][key]
	}
	if !ok {
		return string(key)
	}
	localizedArgs := make([]any, len(args))
	for i, arg := range args {
		switch value := arg.(type) {
		case Message:
			localizedArgs[i] = Translate(locale, value.Key, value.Args...)
		case error:
			localizedArgs[i] = ErrorText(locale, value)
		default:
			localizedArgs[i] = value
		}
	}
	return fmt.Sprintf(template, localizedArgs...)
}

func Text(locale Locale, message Message) string {
	return Translate(locale, message.Key, message.Args...)
}

func ErrorText(locale Locale, err error) string {
	if err == nil {
		return ""
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		parts := make([]string, 0)
		for _, item := range joined.Unwrap() {
			if item != nil {
				parts = append(parts, ErrorText(locale, item))
			}
		}
		return strings.Join(parts, "\n")
	}
	var localized *LocalizedError
	if errors.As(err, &localized) {
		args := append([]any(nil), localized.Message.Args...)
		if localized.Cause != nil && localized.causeArgument {
			args = append(args, localized.Cause)
		}
		return Translate(locale, localized.Message.Key, args...)
	}
	return err.Error()
}

func loadCatalogs() map[Locale]map[Key]string {
	result := make(map[Locale]map[Key]string, 2)
	for _, locale := range []Locale{Chinese, English} {
		data, err := fs.ReadFile(localeFiles, "locales/"+string(locale)+".json")
		if err != nil {
			panic(err)
		}
		messages := make(map[string]string)
		if err := json.Unmarshal(data, &messages); err != nil {
			panic(err)
		}
		catalog := make(map[Key]string, len(messages))
		for key, value := range messages {
			catalog[Key(key)] = value
		}
		result[locale] = catalog
	}
	for key := range result[Chinese] {
		if _, ok := result[English][key]; !ok {
			panic(fmt.Sprintf("missing English translation for %q", key))
		}
	}
	for key := range result[English] {
		if _, ok := result[Chinese][key]; !ok {
			panic(fmt.Sprintf("missing Chinese translation for %q", key))
		}
	}
	return result
}
