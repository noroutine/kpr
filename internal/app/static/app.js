// Simple JavaScript for testing API endpoints

function testHello() {
    const output = document.getElementById('output');
    output.textContent = 'Loading...';

    fetch('/api/hello')
        .then(response => response.json())
        .then(data => {
            output.textContent = JSON.stringify(data, null, 2);
        })
        .catch(error => {
            output.textContent = 'Error: ' + error.message;
        });
}

function testData() {
    const output = document.getElementById('output');
    output.textContent = 'Loading...';

    fetch('/api/data')
        .then(response => response.json())
        .then(data => {
            output.textContent = JSON.stringify(data, null, 2);
        })
        .catch(error => {
            output.textContent = 'Error: ' + error.message;
        });
}

// Optional: Load initial data on page load
document.addEventListener('DOMContentLoaded', function() {
    console.log('kpr SPA loaded successfully');
});
