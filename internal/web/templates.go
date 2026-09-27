package web

const indexTemplate = `<!DOCTYPE html>
<html>
<head>
    <title>kpr - {{ .Hostname }}</title>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        body { font-family: monospace; margin: 20px; background: #1e1e1e; color: #d4d4d4; }
        h1 { color: #4ec9b0; margin-bottom: 5px; }
        .hostname { color: #808080; font-size: 0.9em; margin-bottom: 20px; }
        h2 { color: #dcdcaa; margin-top: 20px; margin-bottom: 10px; font-size: 1.2em; }
        .section { margin-bottom: 30px; }
        .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(300px, 1fr)); gap: 15px; }
        .card {
            background: #2d2d30;
            border-left: 4px solid #007acc;
            padding: 16px;
        }
        .stat-card {
            border-left-color: #4ec9b0;
        }
        .config-card {
            border-left-color: #dcdcaa;
        }
        .label { color: #ce9178; font-size: 0.85em; margin-bottom: 4px; }
        .value { color: #9cdcfe; font-size: 1.2em; font-weight: bold; }
        .meta { color: #808080; font-size: 0.9em; margin-top: 4px; }
        .config-item {
            margin-bottom: 12px;
            padding-bottom: 8px;
            border-bottom: 1px solid #3e3e42;
        }
        .config-item:last-child {
            border-bottom: none;
            margin-bottom: 0;
            padding-bottom: 0;
        }
        .config-key { color: #ce9178; font-size: 0.9em; }
        .config-value { color: #9cdcfe; margin-top: 4px; }
        a { color: #569cd6; text-decoration: none; }
        a:hover { text-decoration: underline; }
        hr { margin: 30px 0; border: 0; border-top: 1px solid #3e3e42; }
        ul { line-height: 1.6; }
        .footer { margin-top: 40px; text-align: center; color: #808080; font-size: 0.85em; }
    </style>
</head>
<body>
    <h1>📋 kpr - Management Console</h1>
    <div class="hostname">{{ .Hostname }}</div>

    <div class="section">
        <h2>📊 Application Metrics</h2>
        <div class="grid">
            <div class="card stat-card">
                <div class="label">Uptime</div>
                <div class="value">{{ .Uptime }}</div>
                <div class="meta">Since startup</div>
            </div>
            <div class="card stat-card">
                <div class="label">API Requests</div>
                <div class="value">{{ .Requests }}</div>
                <div class="meta">Application server (port 8080)</div>
            </div>
            <div class="card stat-card">
                <div class="label">Go Runtime</div>
                <div class="value">{{ .GoVersion }}</div>
                <div class="meta">Running version</div>
            </div>
        </div>
    </div>

    {{ if .Links }}
    <div class="section">
        <h2>🔭 Observability</h2>
        <div class="grid">
            {{ range .Links }}
            <div class="card">
                <div class="value"><a href="{{ .URL }}" target="_blank" rel="noopener">{{ .Name }}</a></div>
                <div class="meta">{{ .Blurb }}</div>
            </div>
            {{ end }}
        </div>
    </div>
    {{ end }}

    <div class="section">
        <h2>⚙️ Configuration</h2>
        <div class="grid">
            <div class="card config-card">
                <div class="config-item">
                    <div class="config-key">Version</div>
                    <div class="config-value">{{ .Version }}</div>
                </div>
                <div class="config-item">
                    <div class="config-key">Commit</div>
                    <div class="config-value">{{ .Commit }}</div>
                </div>
                <div class="config-item">
                    <div class="config-key">Built At</div>
                    <div class="config-value">{{ .BuildTime }}</div>
                </div>
            </div>
            <div class="card config-card">
                <div class="config-item">
                    <div class="config-key">Hostname</div>
                    <div class="config-value">{{ .Hostname }}</div>
                </div>
                <div class="config-item">
                    <div class="config-key">Management Console</div>
                    <div class="config-value">{{ .ManagementHost }}:{{ .ManagementPort }}</div>
                </div>
                <div class="config-item">
                    <div class="config-key">Application Server</div>
                    <div class="config-value">{{ .AppHost }}:{{ .AppPort }}</div>
                </div>
            </div>
        </div>
    </div>

    <hr>
    <h2>🔌 API Endpoints</h2>
    <ul>
        <li><a href="/metrics">/metrics</a> - JSON metrics endpoint</li>
        <li><a href="/health">/health</a> - Health check</li>
    </ul>

    <div class="footer">
        kpr {{ .Version }} - Go Application Blueprint
    </div>
</body>
</html>
`
