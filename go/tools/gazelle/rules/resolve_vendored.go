package rules

// vendoredResolver resolves external packages as packages in vendor/.
type vendoredResolver struct{}

func (v vendoredResolver) resolve(importpath, dir string) (label, error) {
	// TODO: Only return this if this should be vendored...
	return label{
		pkg:  "vendor/" + importpath,
		name: defaultLibName,
	}, nil
}
