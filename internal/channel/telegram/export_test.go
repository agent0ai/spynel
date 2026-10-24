package telegram

// SetTestEndpoint keeps synthetic connected-flow tests off the real provider.
func (b *Bot) SetTestEndpoint(url string) { b.baseURL = url }
