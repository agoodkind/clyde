package codestore

import (
	"fmt"

	"goodkind.io/lm-semantic-search/collection"
)

type truth int

const (
	truthFalse truth = iota
	truthTrue
	truthUnknown
)

// predicate evaluates a filter against the row at position. A comparison on a
// null or absent value is unknown, and only a true result selects the row.
type predicate func(position int) truth

func (stored *codeCollection) compileFilter(filter *collection.Filter) (predicate, error) {
	if filter == nil {
		return func(int) truth { return truthTrue }, nil
	}
	switch filter.Kind {
	case collection.FilterAll, collection.FilterAny:
		children := make([]predicate, 0, len(filter.Children))
		for position := range filter.Children {
			child, err := stored.compileFilter(&filter.Children[position])
			if err != nil {
				return nil, err
			}
			children = append(children, child)
		}
		if filter.Kind == collection.FilterAll {
			return allOf(children), nil
		}
		return anyOf(children), nil
	case collection.FilterNot:
		if len(filter.Children) != 1 {
			return nil, fmt.Errorf("not filter has %d children, want 1", len(filter.Children))
		}
		child, err := stored.compileFilter(&filter.Children[0])
		if err != nil {
			return nil, err
		}
		return func(position int) truth {
			switch child(position) {
			case truthTrue:
				return truthFalse
			case truthFalse:
				return truthTrue
			case truthUnknown:
				return truthUnknown
			}
			return truthUnknown
		}, nil
	case collection.FilterEquals, collection.FilterIn, collection.FilterRange, collection.FilterIsNull, collection.FilterIsPresent:
		return stored.compileLeaf(filter)
	default:
		return nil, fmt.Errorf("unsupported filter kind %q", filter.Kind)
	}
}

func allOf(children []predicate) predicate {
	return func(position int) truth {
		result := truthTrue
		for _, child := range children {
			switch child(position) {
			case truthFalse:
				return truthFalse
			case truthUnknown:
				result = truthUnknown
			case truthTrue:
			}
		}
		return result
	}
}

func anyOf(children []predicate) predicate {
	return func(position int) truth {
		result := truthFalse
		for _, child := range children {
			switch child(position) {
			case truthTrue:
				return truthTrue
			case truthUnknown:
				result = truthUnknown
			case truthFalse:
			}
		}
		return result
	}
}

func (stored *codeCollection) compileLeaf(filter *collection.Filter) (predicate, error) {
	column, declared := stored.columnIndex[filter.Column]
	if !declared {
		return nil, fmt.Errorf("filter column %q is not declared", filter.Column)
	}
	values := stored.dictionaries[column]
	present := func(position int) (collection.ScalarValue, bool) {
		id := stored.rows[position].cells[column]
		if id == 0 || values.values[id].Null {
			return collection.EmptyScalar(), false
		}
		return values.values[id], true
	}
	switch filter.Kind {
	case collection.FilterIsNull:
		return func(position int) truth {
			if _, found := present(position); found {
				return truthFalse
			}
			return truthTrue
		}, nil
	case collection.FilterIsPresent:
		return func(position int) truth {
			if _, found := present(position); found {
				return truthTrue
			}
			return truthFalse
		}, nil
	case collection.FilterRange:
		return func(position int) truth {
			value, found := present(position)
			if !found {
				return truthUnknown
			}
			if (filter.Lower != nil && value.Int64 < *filter.Lower) || (filter.Upper != nil && value.Int64 >= *filter.Upper) {
				return truthFalse
			}
			return truthTrue
		}, nil
	case collection.FilterAll, collection.FilterAny, collection.FilterNot:
		return nil, fmt.Errorf("filter kind %q is not a leaf", filter.Kind)
	case collection.FilterEquals, collection.FilterIn:
		wanted := make(map[collection.ScalarValue]struct{}, len(filter.Values))
		for _, value := range filter.Values {
			wanted[value] = struct{}{}
		}
		return func(position int) truth {
			value, found := present(position)
			if !found {
				return truthUnknown
			}
			if _, match := wanted[value]; match {
				return truthTrue
			}
			return truthFalse
		}, nil
	}
	return nil, fmt.Errorf("unsupported filter kind %q", filter.Kind)
}
