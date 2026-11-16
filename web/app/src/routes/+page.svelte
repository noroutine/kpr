<script lang="ts">
	import { onDestroy, onMount } from 'svelte';

	interface Status {
		status: string;
		store: string;
	}

	let status: Status | null = null;
	let error: string | null = null;
	let timer: ReturnType<typeof setInterval> | null = null;

	async function load(): Promise<void> {
		try {
			const r = await fetch('/api/status');
			if (!r.ok) throw new Error(`status: ${r.status}`);
			status = (await r.json()) as Status;
			error = null;
		} catch (e) {
			error = e instanceof Error ? e.message : 'fetch failed';
		}
	}

	onMount(() => {
		void load();
		timer = setInterval(() => void load(), 15000);
	});

	onDestroy(() => {
		if (timer) clearInterval(timer);
	});
</script>

<svelte:head>
	<title>kpr · keeper for your homelab registry</title>
</svelte:head>

<div class="min-h-screen bg-gray-50 text-gray-900">
	<div class="mx-auto max-w-3xl px-6 py-8">
		<header class="flex items-baseline justify-between">
			<div>
				<h1 class="text-2xl font-semibold">kpr</h1>
				<p class="mt-1 text-sm text-gray-500">keeper for your homelab registry</p>
			</div>
			{#if error}
				<span class="rounded bg-red-100 px-2 py-1 text-xs font-medium text-red-800">{error}</span>
			{:else if status}
				<span
					class="rounded px-2 py-1 text-xs font-medium {status.status === 'ok'
						? 'bg-green-100 text-green-800'
						: 'bg-amber-100 text-amber-800'}"
				>
					{status.status}
				</span>
			{:else}
				<span class="rounded bg-gray-100 px-2 py-1 text-xs font-medium text-gray-600">loading…</span>
			{/if}
		</header>

		<section class="mt-8 rounded-lg bg-white p-6 shadow-sm">
			<h2 class="text-xl font-semibold">Push. Pull. Forget.</h2>
			<p class="mt-2 text-sm text-gray-600">
				Just enough on top of your registry to stop your disk — and your life — from bloating.
				<br />Push with a TTL, pull while it lasts, forget the rest.
				<br />Your registry, your disk back.
			</p>
			<pre
				class="mt-4 overflow-x-auto rounded-lg border border-gray-200 bg-gray-100 p-4 font-mono text-sm text-gray-800">$ docker tag my-app:build my-registry:5000/my-app:10m
$ docker push my-registry:5000/my-app:10m
# …10 minutes later it's due — the next sweep deletes it.</pre>
		</section>

		<footer class="mt-8 text-xs text-gray-400">kpr · reap plans, sweep deletes</footer>
	</div>
</div>
