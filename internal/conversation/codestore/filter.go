package codestore

import (
	"fmt"

	"goodkind.io/lm-semantic-search/collection"
)

type truth uint8

const (
	truthFalse truth = iota
	truthTrue
	truthUnknown
)

// Comparisons with null or absent values are unknown, except for the explicit
// presence predicates. Only a true result selects a row.
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
	decide, err := leafDecision(filter)
	if err != nil {
		return nil, err
	}
	values := stored.dictionaries[column].values
	decisions := make([]truth, len(values))
	decisions[0] = decide(collection.EmptyScalar(), false)
	for id := 1; id < len(values); id++ {
		decisions[id] = decide(values[id], !values[id].Null)
	}
	return func(position int) truth {
		return decisions[stored.rows[position].cells[column]]
	}, nil
}

func leafDecision(filter *collection.Filter) (func(value collection.ScalarValue, present bool) truth, error) {
	switch filter.Kind {
	case collection.FilterIsNull:
		return func(_ collection.ScalarValue, present bool) truth {
			if present {
				return truthFalse
			}
			return truthTrue
		}, nil
	case collection.FilterIsPresent:
		return func(_ collection.ScalarValue, present bool) truth {
			if present {
				return truthTrue
			}
			return truthFalse
		}, nil
	case collection.FilterRange:
		return func(value collection.ScalarValue, present bool) truth {
			if !present {
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
		return func(value collection.ScalarValue, present bool) truth {
			if !present {
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
