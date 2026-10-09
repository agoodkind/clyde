package vectorsearch

// RanksEveryAllowedPassage returns whether the client uses a local store
// that ranks every allowed passage without a fixed depth.
// A nil client returns false.
func (c *Client) RanksEveryAllowedPassage() bool {
	if c == nil {
		return false
	}
	_, local := c.store.(*localBackend)
	return local
}
