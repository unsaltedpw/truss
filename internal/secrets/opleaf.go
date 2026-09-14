package secrets

import "context"

// ReadFields returns the non-secret fields of one item, and whether the item
// exists at all.
//
// It exists because Field's contract cannot answer "has this been provisioned
// yet": Field treats a missing field, an empty field and an absent item as
// errors, which is right for a publisher fetching a credential it is about to
// write (an empty value there is a bug, not a skip) and wrong for a caller
// deciding whether to mint. Conflating them would mean distinguishing "no key
// yet" from "1Password unreachable" by matching error text, and an error-text
// comparison is exactly how a transport failure quietly becomes a "not found".
//
// So the three answers are three values here: (nil, false, nil) means the vault
// is reachable and has no such item; (fields, true, nil) means it does; any
// non-nil error means nothing can be concluded, which is the only case a caller
// may not act on.
//
// ⚠️ CONCEALED fields are never returned. A caller deciding whether a key
// exists does not need the private half, and a method that handed it back would
// put it one `json.Marshal` away from a log line.
func (o *OP) ReadFields(ctx context.Context, item string) (map[string]string, bool, error) {
	tok, err := o.token()
	if err != nil {
		return nil, false, err
	}
	base, err := apiBaseFromToken(tok)
	if err != nil {
		return nil, false, err
	}
	vaultID, err := o.findVault(ctx, base, tok)
	if err != nil {
		return nil, false, err
	}
	existing, found, err := o.findItem(ctx, base, tok, vaultID, item)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	out := map[string]string{}
	for _, s := range existing.Sections {
		for _, f := range s.Fields {
			if f.Type == "CONCEALED" {
				continue
			}
			out[f.Label] = f.Value
		}
	}
	return out, true, nil
}
