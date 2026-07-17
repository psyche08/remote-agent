package buildinfo

var (
	Version = "dev"
	Commit  = "dev"
	BuiltAt = ""
)

func Info() map[string]any {
	return map[string]any{
		"version":  Version,
		"commit":   Commit,
		"built_at": BuiltAt,
	}
}
