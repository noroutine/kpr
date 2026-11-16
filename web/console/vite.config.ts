import tailwindcss from '@tailwindcss/vite';
import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [
		tailwindcss(),
		sveltekit({
			adapter: adapter({
				pages: 'build',
				assets: 'build',
				fallback: undefined,
				strict: true
			})
		})
	],
	server: {
		proxy: {
			'/metrics': 'http://127.0.0.1:9300',
			'/health': 'http://127.0.0.1:9300',
			'/api': 'http://127.0.0.1:9300'
		}
	}
});
