package httpapi

import _ "embed"

// consolePage is the web console served at / (see console.html): a SQL
// editor, the table list and status, using this API with the user's
// credentials.
//
//go:embed console.html
var consolePage []byte
