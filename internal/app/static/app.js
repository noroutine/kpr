// kpr front page: one real call. The status ball reads GET /health
// (process alive, build version, receiver redis reachability):
// ok is green, degraded is amber, an unreachable keeper is red.

function setStatus(ballClass, text, version) {
    document.getElementById('ball').className = 'ball ' + ballClass;
    document.getElementById('status-text').textContent = text;
    document.getElementById('version').textContent = version || 'dev';
}

document.addEventListener('DOMContentLoaded', function() {
    fetch('/health')
        .then(response => response.json())
        .then(data => {
            var ball = data.status === 'ok' ? 'ok' : 'warn';
            setStatus(ball, 'keeper ' + data.status + ' · redis ' + data.redis, data.version);
        })
        .catch(() => {
            setStatus('down', 'keeper unreachable', 'unknown');
        });
});
