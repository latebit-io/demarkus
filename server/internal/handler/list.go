package handler

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
)

const (
	listCursorVersion = byte(1)
	listCursorArchive = byte(1)
	listTruncatedNote = "\n*...truncated, too many entries*\n"
)

var errListPageCannotProgress = errors.New("one LIST entry exceeds the response size limit")

type directoryPage struct {
	Body       string
	EntryCount int
	Complete   bool
	LastName   string
}

func parseListPageSize(raw string) (int, error) {
	if raw == "" {
		return MaxDirectoryEntries, nil
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size < 1 || size > MaxDirectoryEntries {
		return 0, fmt.Errorf("page-size must be between 1 and %d", MaxDirectoryEntries)
	}
	return size, nil
}

func parseListIncludeArchived(raw string) (bool, error) {
	switch raw {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("include-archived must be true or false")
	}
}

func encodeListCursor(reqPath string, includeArchived bool, after string) (string, error) {
	if after == "" {
		return "", errors.New("LIST cursor requires a non-empty entry name")
	}
	if len(reqPath) > protocol.MaxRequestPathLength || len(reqPath) > int(^uint16(0)) {
		return "", errors.New("LIST cursor path exceeds limit")
	}
	flags := byte(0)
	if includeArchived {
		flags = listCursorArchive
	}
	raw := make([]byte, 4, 4+len(reqPath)+len(after))
	raw[0] = listCursorVersion
	raw[1] = flags
	binary.BigEndian.PutUint16(raw[2:4], uint16(len(reqPath)))
	raw = append(raw, reqPath...)
	raw = append(raw, after...)
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeListCursor(encoded, reqPath string, includeArchived bool) (string, error) {
	if encoded == "" {
		return "", nil
	}
	maxRaw := 4 + 2*protocol.MaxRequestPathLength
	if base64.RawURLEncoding.DecodedLen(len(encoded)) > maxRaw {
		return "", errors.New("LIST cursor exceeds limit")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) < 5 {
		return "", errors.New("invalid LIST cursor")
	}
	if raw[0] != listCursorVersion || raw[1]&^listCursorArchive != 0 {
		return "", errors.New("unsupported LIST cursor")
	}
	pathLen := int(binary.BigEndian.Uint16(raw[2:4]))
	if pathLen == 0 || 4+pathLen >= len(raw) {
		return "", errors.New("invalid LIST cursor")
	}
	if string(raw[4:4+pathLen]) != reqPath || (raw[1]&listCursorArchive != 0) != includeArchived {
		return "", errors.New("LIST cursor does not match this request")
	}
	after := string(raw[4+pathLen:])
	if after == "" {
		return "", errors.New("invalid LIST cursor")
	}
	return after, nil
}

func buildDirectoryPage(reqPath string, entries []store.DirEntry, after string, pageSize int) (directoryPage, error) {
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Name >= entries[i].Name {
			return directoryPage{}, errors.New("LIST entries are not strictly ordered")
		}
	}

	displayPath := reqPath
	if displayPath != "/" && !strings.HasSuffix(displayPath, "/") {
		displayPath += "/"
	}
	var body strings.Builder
	body.WriteString("\n# Index of " + escapeMD(displayPath) + "\n\n")

	start := sort.Search(len(entries), func(i int) bool { return entries[i].Name > after })
	page := directoryPage{}
	next := start
	for next < len(entries) && page.EntryCount < pageSize {
		line := directoryEntryLine(entries[next])
		moreAfter := next+1 < len(entries)
		reserve := 0
		if moreAfter {
			reserve = len(listTruncatedNote)
		}
		if body.Len()+len(line)+reserve > protocol.MaxBodyLength {
			if page.EntryCount == 0 {
				return directoryPage{}, errListPageCannotProgress
			}
			break
		}
		body.WriteString(line)
		page.EntryCount++
		page.LastName = entries[next].Name
		next++
	}
	page.Complete = next == len(entries)
	if !page.Complete {
		body.WriteString(listTruncatedNote)
	}
	page.Body = body.String()
	return page, nil
}

func directoryEntryLine(entry store.DirEntry) string {
	display := escapeMD(entry.Name)
	link := escapeURL(entry.Name)
	if entry.IsDir {
		return "- [" + display + "/](" + link + "/)\n"
	}
	return "- [" + display + "](" + link + ")\n"
}
