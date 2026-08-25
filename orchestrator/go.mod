module saturday/orchestrator

go 1.26

require (
	saturday/inject v0.0.0-00010101000000-000000000000
	saturday/llmcore v0.0.0-00010101000000-000000000000
	saturday/settle v0.0.0-00010101000000-000000000000
	saturday/stageclient v0.0.0-00010101000000-000000000000
	saturday/watcherclient v0.0.0-00010101000000-000000000000
)

replace (
	saturday/inject => ../inject
	saturday/llmcore => ../llmcore
	saturday/settle => ../settle
	saturday/stageclient => ../stageclient
	saturday/watcherclient => ../watcherclient
)
