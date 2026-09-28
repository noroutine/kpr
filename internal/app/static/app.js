// kpr front page: one real call, repeated. The status ball reads GET
// /health (process alive, build version, receiver redis reachability)
// every 15 seconds: ok is green, degraded is amber, an unreachable
// keeper is red — no reload needed to see it change.

var healthTimer = null;

function setStatus(ballClass, text, version) {
    document.getElementById('ball').className = 'ball ' + ballClass;
    document.getElementById('status-text').textContent = text;
    document.getElementById('version').textContent = version || 'dev';
}

function checkHealth() {
    fetch('/health')
        .then(response => response.json())
        .then(data => {
            var ball = data.status === 'ok' ? 'ok' : 'warn';
            setStatus(ball, 'keeper ' + data.status + ' · redis ' + data.redis, data.version);
        })
        .catch(() => {
            setStatus('down', 'keeper unreachable', 'unknown');
        });
}

document.addEventListener('DOMContentLoaded', function() {
    checkHealth();
    if (healthTimer === null) {
        healthTimer = setInterval(checkHealth, 15000);
    }
});
