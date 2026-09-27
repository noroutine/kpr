// kpr SPA callers: thin fetch wrappers over the mock API plus the
// build version shown in the header (from /api/hello).

function render(data) {
    document.getElementById('output').textContent = JSON.stringify(data, null, 2);
}

function fail(error) {
    document.getElementById('output').textContent = 'Error: ' + error.message;
}

function testHello() {
    document.getElementById('output').textContent = 'Loading...';

    fetch('/api/hello')
        .then(response => response.json())
        .then(render)
        .catch(fail);
}

function testData() {
    document.getElementById('output').textContent = 'Loading...';

    fetch('/api/data')
        .then(response => response.json())
        .then(render)
        .catch(fail);
}

// Show the serving binary's build version in the header; the mock
// callers above stay manual.
document.addEventListener('DOMContentLoaded', function() {
    fetch('/api/hello')
        .then(response => response.json())
        .then(data => {
            document.getElementById('version').textContent = data.version || 'dev';
        })
        .catch(() => {
            document.getElementById('version').textContent = 'unknown';
        });
});
