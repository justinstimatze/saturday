module saturday/saturday-voice

go 1.26

require (
	github.com/gorilla/websocket v1.5.3
	saturday/llmcore v0.0.0-00010101000000-000000000000
	saturday/moshiclient v0.0.0-00010101000000-000000000000
	saturday/orchestrator v0.0.0-00010101000000-000000000000
)

require (
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	saturday/inject v0.0.0-00010101000000-000000000000 // indirect
	saturday/settle v0.0.0-00010101000000-000000000000 // indirect
	saturday/stageclient v0.0.0-00010101000000-000000000000 // indirect
	saturday/watcherclient v0.0.0-00010101000000-000000000000 // indirect
)

replace (
	saturday/inject => ../inject
	saturday/llmcore => ../llmcore
	saturday/moshiclient => ../moshiclient
	saturday/orchestrator => ../orchestrator
	saturday/settle => ../settle
	saturday/stageclient => ../stageclient
	saturday/watcherclient => ../watcherclient
)
