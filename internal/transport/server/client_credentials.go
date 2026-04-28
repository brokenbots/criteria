package servertrans

// client_credentials.go — pre-existing credential injection for crash recovery.

// SetCredentials configures a pre-existing criteria agent identity on the
// client so that crash-recovery resumptions can authenticate without
// re-registering. Must be called before StartStreams.
func (c *Client) SetCredentials(criteriaID, token string) {
	c.criteriaID = criteriaID
	c.token = token
}
