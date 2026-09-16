package graphstore

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
)

const (
	// DefaultNeighborhoodPageSize bounds queries that omit PageSize.
	DefaultNeighborhoodPageSize = 20
	// MaxNeighborhoodPageSize bounds one neighborhood response.
	MaxNeighborhoodPageSize = 100
	// MaxNeighborhoodEdgesPerRow bounds evidence retained for one document.
	MaxNeighborhoodEdgesPerRow = 20
)

// NeighborhoodDirection selects edges relative to the queried document.
type NeighborhoodDirection string

const (
	// NeighborhoodIncoming selects links to the queried document.
	NeighborhoodIncoming NeighborhoodDirection = "incoming"
	// NeighborhoodOutgoing selects links from the queried document.
	NeighborhoodOutgoing NeighborhoodDirection = "outgoing"
	// NeighborhoodBoth selects links in either direction.
	NeighborhoodBoth NeighborhoodDirection = "both"
)

// NeighborhoodOptions controls one bounded neighborhood page. A nil Relations
// slice matches every relation; a non-nil slice matches exact values, including "".
type NeighborhoodOptions struct {
	Direction NeighborhoodDirection
	Relations []string
	PageSize  int
	Cursor    string
}

// NeighborhoodEdge carries an edge and a copy of its source document evidence.
type NeighborhoodEdge struct {
	Edge   graph.Edge
	Source StoredNode
}

// NeighborhoodRow groups bounded edge evidence for one adjacent document.
type NeighborhoodRow struct {
	Node         StoredNode
	Edges        []NeighborhoodEdge
	OmittedEdges int // matching edges excluded by MaxNeighborhoodEdgesPerRow
}

// NeighborhoodPage sorts rows by document URL and each row's edges by
// From, To, then Rel.
type NeighborhoodPage struct {
	Rows       []NeighborhoodRow
	TotalRows  int
	NextCursor string
}

type neighborhoodCursor struct {
	Query string `json:"q"`
	After string `json:"a"`
}

// Neighborhood queries the selected graph state without changing freshness or
// candidate selection. Empty Direction defaults to both.
func (s *Store) Neighborhood(rawURL string, opts NeighborhoodOptions) (NeighborhoodPage, error) {
	query, err := normalizeNeighborhoodQuery(rawURL, opts)
	if err != nil {
		return NeighborhoodPage{}, err
	}
	position, err := decodeNeighborhoodCursor(opts.Cursor, query.identity)
	if err != nil {
		return NeighborhoodPage{}, err
	}

	result := s.neighborhoodRows(query, position)
	page := NeighborhoodPage{Rows: result.rows, TotalRows: result.totalRows}
	if result.hasMore {
		page.NextCursor, err = encodeNeighborhoodCursor(query.identity, page.Rows[len(page.Rows)-1].Node.URL)
		if err != nil {
			return NeighborhoodPage{}, err
		}
	}
	return page, nil
}

type neighborhoodQuery struct {
	url       string
	direction NeighborhoodDirection
	relations map[string]struct{}
	pageSize  int
	identity  string
}

type neighborhoodPosition struct {
	after string
	set   bool
}

type neighborhoodResult struct {
	rows      []NeighborhoodRow
	totalRows int
	hasMore   bool
}

func normalizeNeighborhoodQuery(rawURL string, opts NeighborhoodOptions) (neighborhoodQuery, error) {
	direction := opts.Direction
	if direction == "" {
		direction = NeighborhoodBoth
	}
	if direction != NeighborhoodIncoming && direction != NeighborhoodOutgoing && direction != NeighborhoodBoth {
		return neighborhoodQuery{}, fmt.Errorf("invalid neighborhood direction %q", direction)
	}
	pageSize := opts.PageSize
	if pageSize == 0 {
		pageSize = DefaultNeighborhoodPageSize
	}
	if pageSize < 1 || pageSize > MaxNeighborhoodPageSize {
		return neighborhoodQuery{}, fmt.Errorf("neighborhood page size must be between 1 and %d, or 0 for the default", MaxNeighborhoodPageSize)
	}

	url := links.CanonicalURL(rawURL)
	relations := neighborhoodRelations(opts.Relations)
	identity, err := neighborhoodQueryIdentity(url, direction, opts.Relations)
	if err != nil {
		return neighborhoodQuery{}, fmt.Errorf("identify neighborhood query: %w", err)
	}
	return neighborhoodQuery{
		url: url, direction: direction, relations: relations, pageSize: pageSize,
		identity: identity,
	}, nil
}

func neighborhoodRelations(values []string) map[string]struct{} {
	if values == nil {
		return nil
	}
	relations := make(map[string]struct{}, len(values))
	for _, relation := range values {
		relations[relation] = struct{}{}
	}
	return relations
}

func neighborhoodQueryIdentity(url string, direction NeighborhoodDirection, relations []string) (string, error) {
	identity := struct {
		URL       string                `json:"url"`
		Direction NeighborhoodDirection `json:"direction"`
		Relations []string              `json:"relations"`
	}{URL: url, Direction: direction}
	if relations != nil {
		set := neighborhoodRelationSet(relations)
		identity.Relations = make([]string, 0, len(set))
		for relation := range set {
			identity.Relations = append(identity.Relations, relation)
		}
		slices.Sort(identity.Relations)
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func neighborhoodRelationSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func decodeNeighborhoodCursor(raw, identity string) (neighborhoodPosition, error) {
	if raw == "" {
		return neighborhoodPosition{}, nil
	}
	if len(raw) > 1024 {
		return neighborhoodPosition{}, errors.New("invalid neighborhood cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return neighborhoodPosition{}, errors.New("invalid neighborhood cursor")
	}
	var cursor neighborhoodCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Query == "" {
		return neighborhoodPosition{}, errors.New("invalid neighborhood cursor")
	}
	if cursor.Query != identity {
		return neighborhoodPosition{}, errors.New("neighborhood cursor does not match query")
	}
	return neighborhoodPosition{after: cursor.After, set: true}, nil
}

func encodeNeighborhoodCursor(identity, after string) (string, error) {
	data, err := json.Marshal(neighborhoodCursor{Query: identity, After: after})
	if err != nil {
		return "", fmt.Errorf("encode neighborhood cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (s *Store) neighborhoodRows(query neighborhoodQuery, position neighborhoodPosition) neighborhoodResult {
	s.mu.RLock()
	seenNeighbors := make(map[string]struct{})
	pageURLs := make([]string, 0, query.pageSize+1)
	s.visitNeighborhoodEdgesLocked(query, func(edge *StoredEdge) {
		neighbor := neighborhoodNeighbor(edge, query.url)
		if _, seen := seenNeighbors[neighbor]; seen {
			return
		}
		seenNeighbors[neighbor] = struct{}{}
		if position.set && neighbor <= position.after {
			return
		}
		index, _ := slices.BinarySearch(pageURLs, neighbor)
		if len(pageURLs) == query.pageSize+1 && index == len(pageURLs) {
			return
		}
		pageURLs = slices.Insert(pageURLs, index, neighbor)
		if len(pageURLs) > query.pageSize+1 {
			pageURLs = pageURLs[:query.pageSize+1]
		}
	})

	result := neighborhoodResult{totalRows: len(seenNeighbors), hasMore: len(pageURLs) > query.pageSize}
	if result.hasMore {
		pageURLs = pageURLs[:query.pageSize]
	}
	rows := make(map[string]*NeighborhoodRow, len(pageURLs))
	for _, url := range pageURLs {
		node := StoredNode{URL: url}
		if stored := s.nodes[url]; stored != nil {
			node = *stored
		}
		rows[url] = &NeighborhoodRow{Node: node}
	}
	s.visitNeighborhoodEdgesLocked(query, func(edge *StoredEdge) {
		row := rows[neighborhoodNeighbor(edge, query.url)]
		if row == nil {
			return
		}
		source := StoredNode{URL: edge.From}
		if stored := s.nodes[edge.From]; stored != nil {
			source = *stored
		}
		row.Edges = append(row.Edges, NeighborhoodEdge{
			Edge: graph.Edge{
				From: edge.From, To: edge.To, Rel: edge.Rel,
				Label: edge.Label, Anchor: edge.Anchor, Count: max(edge.Count, 1),
			},
			Source: source,
		})
	})
	s.mu.RUnlock()

	result.rows = make([]NeighborhoodRow, 0, len(pageURLs))
	for _, url := range pageURLs {
		row := rows[url]
		slices.SortFunc(row.Edges, compareNeighborhoodEdges)
		if len(row.Edges) > MaxNeighborhoodEdgesPerRow {
			row.OmittedEdges = len(row.Edges) - MaxNeighborhoodEdgesPerRow
			row.Edges = row.Edges[:MaxNeighborhoodEdgesPerRow]
		}
		result.rows = append(result.rows, *row)
	}
	return result
}

func (s *Store) visitNeighborhoodEdgesLocked(query neighborhoodQuery, visit func(*StoredEdge)) {
	visitIndices := func(indices []int, skipSelfLoops bool) {
		for _, index := range indices {
			edge := &s.edges[index]
			if skipSelfLoops && edge.From == query.url && edge.To == query.url {
				continue
			}
			if query.relations != nil {
				if _, match := query.relations[edge.Rel]; !match {
					continue
				}
			}
			visit(edge)
		}
	}
	if query.direction != NeighborhoodOutgoing {
		visitIndices(s.incoming[query.url], false)
	}
	if query.direction != NeighborhoodIncoming {
		visitIndices(s.outgoing[query.url], query.direction == NeighborhoodBoth)
	}
}

func neighborhoodNeighbor(edge *StoredEdge, queryURL string) string {
	neighbor := edge.To
	if edge.To == queryURL {
		neighbor = edge.From
	}
	return neighbor
}

func compareNeighborhoodEdges(a, b NeighborhoodEdge) int { //nolint:gocritic // slices.SortFunc signature
	if order := strings.Compare(a.Edge.From, b.Edge.From); order != 0 {
		return order
	}
	if order := strings.Compare(a.Edge.To, b.Edge.To); order != 0 {
		return order
	}
	return strings.Compare(a.Edge.Rel, b.Edge.Rel)
}
