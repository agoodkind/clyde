package clispec

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"goodkind.io/clyde/internal/conversation"
)

func conversationLookupError(selector string, err error) error {
	var missing *conversation.NotFoundError
	if errors.As(err, &missing) || status.Code(err) == codes.NotFound {
		return errors.New((&conversation.NotFoundError{Selector: selector}).UserMessage())
	}

	return err
}
