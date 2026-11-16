<script lang="ts">
	import { onDestroy, onMount } from 'svelte';

	interface Metrics {
		uptime: string;
		uptime_seconds: number;
		requests: number;
		version: string;
		commit: string;
		build_time: string;
		environment: Record<string, string>;
	}

	interface PlanRow {
		repo: string;
		tag: string;
		reason: string;
		pushed: string;
	}

	interface BundleCard {
		standing: string;
		name: string;
		subject: string;
		issuer: string;
		days_left: number;
		expires: string;
		lifetime: string;
		identity: string;
		minTLS: string;
		trust_only: boolean;
	}

	interface ObsLink {
		name: string;
		url: string;
		blurb?: string;
	}

	interface Overview {
		registry: { standing: string; url: string; reachable: boolean };
		store: {
			standing: string;
			name: string;
			detail: string;
			healthy: boolean;
			lockNote?: string;
			identity?: string;
			fsNote?: string;
		};
		sentinel: { standing: string; gen?: string; ts?: string };
		edge: { standing: string; open: boolean; deny: boolean; hold: boolean };
		clock: { standing: string; method: string; server: string; skewNanos: number; toleranceNanos: number };
		clockNow: string;
		goVersion: string;
		goOSArch: string;
		managementAddr: string;
		appAddr: string;
		receiverAddr: string;
		obs: { standing: string; links?: ObsLink[] };
		storeOk: boolean;
		registryOk: boolean;
		tracked: number;
		due: number;
		plan: PlanRow[];
		activity: string[];
		activityTotal: number;
		bundles: BundleCard[];
		reloadError: string;
		source: string;
	}

	let metrics: Metrics | null = null;
	let overview: Overview | null = null;
	let error: string | null = null;
	let reloadNote: string | null = null;
	let reloading = false;
	let timer: ReturnType<typeof setInterval> | null = null;

	const badges: Record<string, string> = {
		ok: 'bg-green-100 text-green-800',
		degraded: 'bg-amber-100 text-amber-800',
		hold: 'bg-amber-100 text-amber-800',
		locked: 'bg-amber-100 text-amber-800',
		incomplete: 'bg-amber-100 text-amber-800',
		denied: 'bg-red-100 text-red-800',
		expired: 'bg-red-100 text-red-800',
		unreachable: 'bg-red-100 text-red-800',
		parked: 'bg-gray-100 text-gray-600',
		unproven: 'bg-gray-100 text-gray-600',
		unavailable: 'bg-gray-100 text-gray-600',
		unchecked: 'bg-gray-100 text-gray-600',
		closed: 'bg-gray-100 text-gray-600',
		error: 'bg-red-100 text-red-800'
	};

	function tone(s: string): string {
		return badges[s] ?? 'bg-gray-100 text-gray-600';
	}

	function nanos(ms: number): string {
		if (!ms) return '—';
		const sign = ms < 0 ? '-' : '+';
		const abs = Math.abs(ms);
		if (abs >= 1e9) return `${sign}${(abs / 1e9).toFixed(1)}s`;
		if (abs >= 1e6) return `${sign}${(abs / 1e6).toFixed(1)}ms`;
		return `${sign}${Math.round(abs / 1e3)}µs`;
	}

	function edgePosture(): string {
		if (!overview) return 'closed';
		if (!overview.edge.open) return 'closed';
		return overview.edge.deny ? 'open, deny' : 'open, pass';
	}

	function appStatusURL(): string {
		const port = metrics?.environment['KPR_APP_PORT'];
		if (!port) return '';
		return `http://${window.location.hostname}:${port}/api/status`;
	}

	async function load(): Promise<void> {
		try {
			const [m, o] = await Promise.all([
				fetch('/metrics').then((r) => {
					if (!r.ok) throw new Error(`metrics: ${r.status}`);
					return r.json() as Promise<Metrics>;
				}),
				fetch('/api/overview').then((r) => {
					if (!r.ok) throw new Error(`overview: ${r.status}`);
					return r.json() as Promise<Overview>;
				})
			]);
			metrics = m;
			overview = o;
			error = null;
		} catch (e) {
			error = e instanceof Error ? e.message : 'fetch failed';
		}
	}

	async function reload(): Promise<void> {
		reloading = true;
		reloadNote = null;
		try {
			const r = await fetch('/api/config/reload', { method: 'POST' });
			const body = (await r.json()) as { applied?: number; error?: string; note?: string };
			if (!r.ok) throw new Error(body.error ?? `reload: ${r.status}`);
			reloadNote = `applied ${body.applied ?? 0} bundles${body.note ? ` — ${body.note}` : ''}`;
			await load();
		} catch (e) {
			reloadNote = e instanceof Error ? e.message : 'reload failed';
		} finally {
			reloading = false;
		}
	}

	onMount(() => {
		void load();
		timer = setInterval(() => void load(), 10000);
	});

	onDestroy(() => {
		if (timer) clearInterval(timer);
	});
</script>

<svelte:head>
	<title>Management console</title>
</svelte:head>

<div class="min-h-screen bg-gray-50 text-gray-900">
	{#if metrics && overview}
	<div class="mx-auto max-w-5xl px-6 py-8">
		<header class="flex items-baseline justify-between">
			<div>
				<h1 class="text-2xl font-semibold">Management console</h1>
				<p class="mt-1 text-sm text-gray-500">
					{metrics.version} · {metrics.commit} · built {metrics.build_time}
				</p>
			</div>
			{#if error}
				<span class="rounded px-2 py-1 text-xs font-medium {tone('error')}">{error}</span>
			{:else}
				<span class="rounded px-2 py-1 text-xs font-medium {tone('ok')}">live</span>
			{/if}
		</header>

		<section class="mt-8">
			<h2 class="text-sm font-semibold tracking-wide text-gray-500">SERVERS</h2>
			<div class="mt-3 grid grid-cols-2 gap-4 md:grid-cols-4">
				<div class="rounded-lg bg-white p-4 shadow-sm">
					<div class="text-xs text-gray-500">Uptime</div>
					<div class="mt-1 text-lg font-medium">{metrics.uptime}</div>
				</div>
				<div class="rounded-lg bg-white p-4 shadow-sm">
					<div class="text-xs text-gray-500">API requests</div>
					<div class="mt-1 text-lg font-medium">{metrics.requests}</div>
				</div>
				<div class="rounded-lg bg-white p-4 shadow-sm">
					<div class="text-xs text-gray-500">Listens</div>
					<div class="mt-1 text-lg font-medium">{overview.managementAddr}</div>
					<div class="text-xs text-gray-500">
						app {overview.appAddr} · receiver {overview.receiverAddr}
					</div>
					<div class="text-xs text-gray-500">host {metrics.environment['HOSTNAME'] ?? '—'}</div>
				</div>
				<div class="rounded-lg bg-white p-4 shadow-sm">
					<div class="text-xs text-gray-500">Runtime</div>
					<div class="mt-1 text-lg font-medium">{overview.goVersion}</div>
					<div class="text-xs text-gray-500">{overview.goOSArch}</div>
				</div>
			</div>
		</section>

			<section class="mt-8">
				<h2 class="text-sm font-semibold tracking-wide text-gray-500">KEEPER</h2>
				<div class="mt-3 grid grid-cols-2 gap-4 md:grid-cols-3">
					<div class="rounded-lg bg-white p-4 shadow-sm">
						<div class="flex items-center gap-2">
							<span class="rounded px-2 py-0.5 text-xs font-medium {tone(overview.registry.standing)}">{overview.registry.standing}</span>
							<span class="text-sm font-medium">Registry</span>
						</div>
						<div class="mt-2 text-sm text-gray-600">{overview.registry.url || 'unknown endpoint'}</div>
					</div>
					<div class="rounded-lg bg-white p-4 shadow-sm">
						<div class="flex items-center gap-2">
							<span class="rounded px-2 py-0.5 text-xs font-medium {tone(overview.store.standing)}">{overview.store.standing}</span>
							<span class="text-sm font-medium">Store · {overview.store.name}</span>
						</div>
						<div class="mt-2 text-sm text-gray-600">{overview.store.detail}</div>
						{#if overview.store.lockNote}
							<div class="mt-1 text-xs text-gray-500">{overview.store.lockNote}</div>
						{/if}
						{#if overview.store.identity}
							<div class="mt-1 text-xs text-gray-500">identity {overview.store.identity}</div>
						{/if}
					</div>
					<div class="rounded-lg bg-white p-4 shadow-sm">
						<div class="flex items-center gap-2">
							<span class="rounded px-2 py-0.5 text-xs font-medium {tone(overview.sentinel.standing)}">{overview.sentinel.standing}</span>
							<span class="text-sm font-medium">Same-store proof</span>
						</div>
						<div class="mt-2 text-sm text-gray-600">
							{#if overview.sentinel.gen}
								{overview.sentinel.gen} · proven {overview.sentinel.ts}
							{:else}
								no generation served yet
							{/if}
						</div>
					</div>
					<div class="rounded-lg bg-white p-4 shadow-sm">
						<div class="flex items-center gap-2">
							<span class="rounded px-2 py-0.5 text-xs font-medium {tone(overview.edge.standing)}">{overview.edge.standing}</span>
							<span class="text-sm font-medium">Edge</span>
						</div>
						<div class="mt-2 text-sm text-gray-600">{edgePosture()}</div>
					</div>
					<div class="rounded-lg bg-white p-4 shadow-sm">
						<div class="flex items-center gap-2">
							<span class="rounded px-2 py-0.5 text-xs font-medium {tone(overview.clock.standing)}">{overview.clock.standing}</span>
							<span class="text-sm font-medium">Clock · {overview.clock.method}</span>
						</div>
						<div class="mt-2 text-2xl font-medium">{nanos(overview.clock.skewNanos)}</div>
						<div class="text-xs text-gray-500">server {overview.clockNow}</div>
					</div>
					<div class="rounded-lg bg-white p-4 shadow-sm">
						<div class="text-xs text-gray-500">Tracked</div>
						<div class="mt-1 text-lg font-medium">{overview.tracked} rows · {overview.due} due</div>
						<div class="text-xs text-gray-500">activity {overview.activityTotal} recorded</div>
					</div>
				</div>
			</section>

			{#if (overview.plan ?? []).length > 0}
				<section class="mt-8">
					<h2 class="text-sm font-semibold tracking-wide text-gray-500">PLAN · {overview.due} DUE</h2>
					<div class="mt-3 overflow-hidden rounded-lg bg-white shadow-sm">
						{#each overview.plan ?? [] as row (`${row.repo}:${row.tag}`)}
							<div class="flex items-baseline justify-between gap-4 border-b border-gray-100 px-4 py-2 last:border-0">
								<span class="text-sm font-medium">{row.repo}:{row.tag}</span>
								<span class="truncate text-xs text-gray-500">{row.reason} · {row.pushed}</span>
							</div>
						{/each}
					</div>
				</section>
			{/if}

			{#if (overview.activity ?? []).length > 0}
				<section class="mt-8">
					<h2 class="text-sm font-semibold tracking-wide text-gray-500">ACTIVITY · LAST {(overview.activity ?? []).length} OF {overview.activityTotal}</h2>
					<pre
						class="mt-3 overflow-x-auto rounded-lg bg-white p-4 font-mono text-sm text-gray-600 shadow-sm">{(overview.activity ?? []).join('\n')}</pre>
				</section>
			{/if}

			{#if overview.obs.links && overview.obs.links.length > 0}
				<section class="mt-8">
					<h2 class="text-sm font-semibold tracking-wide text-gray-500">OBSERVABILITY</h2>
					<div class="mt-3 grid gap-4 md:grid-cols-2">
						{#each overview.obs.links as link (link.name)}
							<div class="rounded-lg bg-white p-4 shadow-sm">
								<div class="text-sm font-medium"><a class="text-blue-700 underline" rel="external" href={link.url} target="_blank">{link.name}</a></div>
								{#if link.blurb}
									<div class="mt-1 text-xs text-gray-500">{link.blurb}</div>
								{/if}
							</div>
						{/each}
					</div>
				</section>
			{/if}

			{#if (overview.bundles ?? []).length > 0}
				<section class="mt-8">
					<h2 class="text-sm font-semibold tracking-wide text-gray-500">TLS BUNDLES</h2>
					<div class="mt-3 grid gap-4 md:grid-cols-2">
						{#each overview.bundles ?? [] as b (b.name)}
							<div class="rounded-lg bg-white p-4 shadow-sm">
								<div class="flex items-center gap-2">
									<span class="rounded px-2 py-0.5 text-xs font-medium {tone(b.standing)}">{b.standing}</span>
									<span class="font-medium">{b.name}</span>
								</div>
								<div class="mt-2 text-sm text-gray-600">{b.identity}</div>
								<div class="mt-1 text-sm text-gray-600">{b.lifetime}</div>
								{#if b.minTLS}
									<div class="mt-1 text-xs text-gray-500">min TLS {b.minTLS}</div>
								{/if}
							</div>
						{/each}
					</div>
				</section>
			{/if}

			{#if overview.reloadError}
				<section class="mt-8">
					<h2 class="text-sm font-semibold tracking-wide text-gray-500">RELOAD</h2>
					<div class="mt-3 rounded-lg bg-white p-4 shadow-sm">
						<span class="rounded px-2 py-0.5 text-xs font-medium {tone('error')}">refused</span>
						<p class="mt-2 text-sm text-gray-600">{overview.reloadError} — old snapshot still serving</p>
					</div>
				</section>
			{/if}

			<section class="mt-8 flex items-center gap-3">
				<button
					class="rounded bg-gray-900 px-4 py-2 text-sm font-medium text-white disabled:opacity-50"
					disabled={reloading}
					onclick={() => void reload()}
				>
					{reloading ? 'Reloading…' : 'Reload config'}
				</button>
				{#if reloadNote}
					<span class="text-sm text-gray-600">{reloadNote}</span>
				{/if}
			</section>

			<section class="mt-8">
				<h2 class="text-sm font-semibold tracking-wide text-gray-500">ENDPOINTS</h2>
				<ul class="mt-3 space-y-1 text-sm">
					<li><a class="text-blue-700 underline" rel="external" href="/metrics">/metrics</a> — JSON metrics</li>
					<li><a class="text-blue-700 underline" rel="external" href="/health">/health</a> — health check</li>
					<li><a class="text-blue-700 underline" rel="external" href="/api/overview">/api/overview</a> — keeper and bundle cards</li>
					<li><a class="text-blue-700 underline" rel="external" href="/api/activity">/api/activity</a> — full activity ring</li>
					{#if metrics && appStatusURL()}
						<li><a class="text-blue-700 underline" rel="external" href={appStatusURL()} target="_blank">app /api/status</a> — expected state, on the app port</li>
					{/if}
				</ul>
			</section>
		</div>
	{:else if error}
		<div class="mx-auto max-w-5xl px-6 py-8">
			<h1 class="text-2xl font-semibold">Management console</h1>
			<p class="mt-4 text-sm text-gray-600">The console API is unreachable: {error}</p>
			<button
				class="mt-4 rounded bg-gray-900 px-4 py-2 text-sm font-medium text-white"
				onclick={() => void load()}
			>
				Retry
			</button>
		</div>
	{/if}
</div>
