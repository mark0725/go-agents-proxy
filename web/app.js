function adminApp() {
    return {
        tab: 'routes',
        authenticated: false,
        apiKey: '',
        error: '',
        routes: {},
        providers: {},
        configMessage: '',
        configError: false,
        llmLogs: [],
        llmDate: new Date().toISOString().split('T')[0],
        llmOffset: 0,
        llmLimit: 50,
        llmTotal: 0,
        appLogs: [],
        appLogAutoRefresh: false,
        appLogInterval: null,
        providerFilter: '',
        providerTypeFilter: '',
        routeFilter: '',
        expandedProviders: {},
        expandedRoutes: {},
        logSortField: 'timestamp',
        logSortDesc: true,
        logStatusFilter: '',
        darkMode: false,
        testRouteId: '',
        testModel: '',
        testPrompt: 'Hello!',
        testUseV1: true,
        testResult: '',
        testLoading: false,
        testError: '',
        sidebarOpen: false,
        selectedRoute: '',
        selectedProvider: '',
        visibleApiKeys: {},

        // Config editing state
        configApp: { level: 'info', auth: true, listen: '0.0.0.0', port: '8082' },
        configUsers: [],
        configTokens: [],
        configRoutes: {},
        configProviders: {},

        async initApp() {
            const dm = localStorage.getItem('proxy_dark_mode');
            if (dm === 'true') { this.darkMode = true; document.documentElement.classList.add('dark'); }

            // Try unauthenticated first to detect auth=false
            try {
                const res = await fetch('/api/routes');
                if (res.ok) {
                    this.authenticated = true;
                    this.routes = await res.json();
                    await this.loadProviders();
                    await this.loadConfig();
                    await this.loadLLMLogs();
                    return;
                }
            } catch (e) {
                // ignore, fall through to key-based login
            }

            const key = localStorage.getItem('proxy_api_key');
            if (key) {
                this.apiKey = key;
                await this.login();
            }
        },

        async login() {
            this.error = '';
            try {
                const res = await fetch('/api/routes', {
                    headers: { 'x-api-key': this.apiKey }
                });
                if (res.ok) {
                    this.authenticated = true;
                    localStorage.setItem('proxy_api_key', this.apiKey);
                    await this.loadRoutes();
                    await this.loadProviders();
                    await this.loadConfig();
                    await this.loadLLMLogs();
                } else {
                    this.error = 'Invalid API key';
                }
            } catch (e) {
                this.error = 'Connection error: ' + e.message;
            }
        },

        logout() {
            this.authenticated = false;
            this.apiKey = '';
            localStorage.removeItem('proxy_api_key');
        },

        toggleDarkMode() {
            this.darkMode = !this.darkMode;
            localStorage.setItem('proxy_dark_mode', this.darkMode);
            if (this.darkMode) {
                document.documentElement.classList.add('dark');
            } else {
                document.documentElement.classList.remove('dark');
            }
        },

        async loadRoutes() {
            const res = await fetch('/api/routes', {
                headers: { 'x-api-key': this.apiKey }
            });
            this.routes = await res.json();
        },

        async loadProviders() {
            const res = await fetch('/api/providers', {
                headers: { 'x-api-key': this.apiKey }
            });
            this.providers = await res.json();
        },

        async loadConfig() {
            const res = await fetch('/api/config', {
                headers: { 'x-api-key': this.apiKey }
            });
            const data = await res.json();
            this.configApp = { ...data.app };
            this.configUsers = JSON.parse(JSON.stringify(data.users || []));
            this.configTokens = JSON.parse(JSON.stringify(data.tokens || []));
            this.configRoutes = JSON.parse(JSON.stringify(data.routes || {}));
            this.configProviders = JSON.parse(JSON.stringify(data.providers || {}));
            // Normalize routes: ensure api_name defaults to 'default' and target.enable defaults to true
            for (const route of Object.values(this.configRoutes)) {
                if (!route.targets) continue;
                for (const target of route.targets) {
                    if (target.enable === undefined || target.enable === null) target.enable = true;
                    if (!target.models) continue;
                    for (const m of target.models) {
                        if (m.api_name === undefined || m.api_name === null) m.api_name = 'default';
                    }
                }
            }
            this.selectDefaultConfigItems();
        },

        selectDefaultConfigItems() {
            const routeKeys = Object.keys(this.configRoutes || {});
            if ((!this.selectedRoute || !this.configRoutes[this.selectedRoute]) && routeKeys.length > 0) {
                this.selectedRoute = routeKeys[0];
            }
            if (routeKeys.length === 0) {
                this.selectedRoute = '';
            }

            const providerKeys = Object.keys(this.configProviders || {});
            if ((!this.selectedProvider || !this.configProviders[this.selectedProvider]) && providerKeys.length > 0) {
                this.selectedProvider = providerKeys[0];
            }
            if (providerKeys.length === 0) {
                this.selectedProvider = '';
            }
        },

        buildConfigObject() {
            return {
                app: { ...this.configApp },
                users: JSON.parse(JSON.stringify(this.configUsers)),
                tokens: JSON.parse(JSON.stringify(this.configTokens)),
                routes: JSON.parse(JSON.stringify(this.configRoutes)),
                providers: JSON.parse(JSON.stringify(this.configProviders))
            };
        },

        async saveConfig() {
            this.configMessage = '';
            try {
                const bodyObj = this.buildConfigObject();
                const res = await fetch('/api/config', {
                    method: 'POST',
                    headers: {
                        'x-api-key': this.apiKey,
                        'Content-Type': 'application/json'
                    },
                    body: JSON.stringify(bodyObj)
                });
                if (res.ok) {
                    this.configMessage = 'Config saved successfully';
                    this.configError = false;
                    await this.loadRoutes();
                    await this.loadProviders();
                } else {
                    const err = await res.json();
                    this.configMessage = err.error?.message || 'Save failed';
                    this.configError = true;
                }
            } catch (e) {
                this.configMessage = 'Error: ' + e.message;
                this.configError = true;
            }
        },

        // Users
        addUser() {
            this.configUsers.push({ name: '', token: '', password: '' });
        },
        removeUser(idx) {
            this.configUsers.splice(idx, 1);
        },

        // Tokens
        addToken() {
            this.configTokens.push({ id: '', token: '' });
        },
        removeToken(idx) {
            this.configTokens.splice(idx, 1);
        },

        // Routes editing
        addRoute() {
            const id = 'new-route-' + Date.now();
            this.configRoutes = { ...this.configRoutes, [id]: { api_type: 'anthropic', targets: [] } };
            this.selectedRoute = id;
        },
        removeRoute(id) {
            const copy = { ...this.configRoutes };
            delete copy[id];
            this.configRoutes = copy;
            this.selectDefaultConfigItems();
        },
        renameRoute(oldId, newId) {
            newId = newId.trim();
            if (!newId || newId === oldId) return;
            if (this.configRoutes[newId]) {
                alert('Route ID already exists');
                return;
            }
            const copy = { ...this.configRoutes };
            copy[newId] = copy[oldId];
            delete copy[oldId];
            this.configRoutes = copy;
            this.selectedRoute = newId;
        },
        addRouteTargetGroup(routeId) {
            const route = this.configRoutes[routeId];
            if (!route) return;
            if (!route.targets) route.targets = [];
            route.targets.push({ name: '', enable: true, models: [] });
        },
        removeRouteTargetGroup(routeId, tIdx) {
            const route = this.configRoutes[routeId];
            if (route && route.targets) route.targets.splice(tIdx, 1);
        },
        addRouteModel(routeId, targetIdx) {
            const route = this.configRoutes[routeId];
            if (!route || !route.targets || !route.targets[targetIdx]) return;
            if (!route.targets[targetIdx].models) route.targets[targetIdx].models = [];
            route.targets[targetIdx].models.push({ match_model: '', provider: '', model_id: '', api_name: 'default' });
        },
        removeRouteModel(routeId, targetIdx, mIdx) {
            const route = this.configRoutes[routeId];
            if (route && route.targets && route.targets[targetIdx] && route.targets[targetIdx].models) {
                route.targets[targetIdx].models.splice(mIdx, 1);
            }
        },

        // Providers editing
        addProvider() {
            const id = 'new-provider-' + Date.now();
            this.configProviders = { ...this.configProviders, [id]: { name: '', enable: false, proxy: '', models: [], apis: [] } };
            this.selectedProvider = id;
        },
        removeProvider(id) {
            const copy = { ...this.configProviders };
            delete copy[id];
            this.configProviders = copy;
            this.selectDefaultConfigItems();
        },
        addProviderModel(providerId) {
            const p = this.configProviders[providerId];
            if (!p) return;
            if (!p.models) p.models = [];
            p.models.push({ model_id: '' });
        },
        removeProviderModel(providerId, idx) {
            const p = this.configProviders[providerId];
            if (p && p.models) p.models.splice(idx, 1);
        },
        addProviderAPI(providerId) {
            const p = this.configProviders[providerId];
            if (!p) return;
            if (!p.apis) p.apis = [];
            p.apis.push({ name: 'default', api_type: 'openai', base_url: '', api_key: '' });
        },
        removeProviderAPI(providerId, idx) {
            const p = this.configProviders[providerId];
            if (p && p.apis) p.apis.splice(idx, 1);
        },

        async loadLLMLogs() {
            const res = await fetch(`/api/logs/llm?date=${this.llmDate}&offset=${this.llmOffset}&limit=${this.llmLimit}`, {
                headers: { 'x-api-key': this.apiKey }
            });
            const data = await res.json();
            this.llmLogs = data.logs || [];
            this.llmTotal = data.total || 0;
        },

        sortedLLMLogs() {
            let logs = [...this.llmLogs];
            if (this.logStatusFilter) {
                const filter = this.logStatusFilter;
                logs = logs.filter(l => {
                    if (filter === 'error') return !!l.error;
                    if (filter === 'success') return !l.error && l.status_code && l.status_code < 400;
                    return true;
                });
            }
            logs.sort((a, b) => {
                let va = a[this.logSortField];
                let vb = b[this.logSortField];
                if (this.logSortField === 'timestamp') {
                    va = new Date(va).getTime();
                    vb = new Date(vb).getTime();
                }
                if (va === undefined) va = '';
                if (vb === undefined) vb = '';
                if (this.logSortDesc) return vb > va ? 1 : -1;
                return va > vb ? 1 : -1;
            });
            return logs;
        },

        async loadAppLogs() {
            const res = await fetch('/api/logs/app?limit=200', {
                headers: { 'x-api-key': this.apiKey }
            });
            const data = await res.json();
            this.appLogs = data.lines || [];
        },

        toggleAppLogAutoRefresh() {
            this.appLogAutoRefresh = !this.appLogAutoRefresh;
            if (this.appLogAutoRefresh) {
                this.loadAppLogs();
                this.appLogInterval = setInterval(() => this.loadAppLogs(), 5000);
            } else {
                if (this.appLogInterval) {
                    clearInterval(this.appLogInterval);
                    this.appLogInterval = null;
                }
            }
        },

        get logPages() {
            return Math.ceil(this.llmTotal / this.llmLimit);
        },

        get currentPage() {
            return Math.floor(this.llmOffset / this.llmLimit) + 1;
        },

        goToPage(p) {
            this.llmOffset = (p - 1) * this.llmLimit;
            this.loadLLMLogs();
        },

        statusBadgeClass(status) {
            if (!status) return 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-300';
            if (status >= 200 && status < 300) return 'bg-green-100 text-green-700 dark:bg-green-900 dark:text-green-300';
            if (status >= 300 && status < 400) return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900 dark:text-yellow-300';
            if (status >= 400 && status < 500) return 'bg-orange-100 text-orange-700 dark:bg-orange-900 dark:text-orange-300';
            return 'bg-red-100 text-red-700 dark:bg-red-900 dark:text-red-300';
        },

        formatDuration(ms) {
            if (ms === undefined || ms === null) return '-';
            if (ms < 1000) return ms + 'ms';
            return (ms / 1000).toFixed(2) + 's';
        },

        formatTokens(n) {
            if (n === undefined || n === null) return '-';
            return n.toLocaleString();
        },

        enabledConfigProviderKeys() {
            const keys = Object.keys(this.configProviders).sort((a, b) => {
                const ea = this.configProviders[a].enable ? 1 : 0;
                const eb = this.configProviders[b].enable ? 1 : 0;
                if (ea !== eb) return eb - ea;
                return a.localeCompare(b);
            });
            return keys;
        },

        enabledConfigProvidersForRoutes() {
            const result = {};
            for (const [name, p] of Object.entries(this.configProviders)) {
                if (p.enable) result[name] = p;
            }
            return result;
        },

        filteredConfigProviders() {
            const filter = this.providerFilter.toLowerCase();
            const typeFilter = this.providerTypeFilter;
            const keys = this.enabledConfigProviderKeys();
            const filtered = {};
            for (const name of keys) {
                const p = this.configProviders[name];
                let match = true;
                if (filter) {
                    match = name.toLowerCase().includes(filter) || (p.name && p.name.toLowerCase().includes(filter));
                }
                if (match && typeFilter) {
                    const hasType = p.apis && p.apis.some(a => a.api_type === typeFilter);
                    if (!hasType) match = false;
                }
                if (match) filtered[name] = p;
            }
            return filtered;
        },

        filteredConfigRoutes() {
            const filter = this.routeFilter.toLowerCase();
            if (!filter) return this.configRoutes;
            const filtered = {};
            for (const [id, r] of Object.entries(this.configRoutes)) {
                if (id.toLowerCase().includes(filter)) {
                    filtered[id] = r;
                    continue;
                }
                if (r.targets) {
                    for (const t of r.targets) {
                        if (t.models) {
                            for (const m of t.models) {
                                if (m.match_model && m.match_model.toLowerCase().includes(filter)) {
                                    filtered[id] = r;
                                    break;
                                }
                            }
                        }
                        if (filtered[id]) break;
                    }
                }
            }
            return filtered;
        },

        filteredProviders() {
            const filter = this.providerFilter.toLowerCase();
            const typeFilter = this.providerTypeFilter;
            const keys = Object.keys(this.providers).sort((a, b) => {
                const pa = this.providers[a];
                const pb = this.providers[b];
                const ea = pa.enable ? 1 : 0;
                const eb = pb.enable ? 1 : 0;
                if (ea !== eb) return eb - ea;
                return a.localeCompare(b);
            });
            const filtered = {};
            for (const name of keys) {
                const p = this.providers[name];
                let match = true;
                if (filter) {
                    match = name.toLowerCase().includes(filter) || (p.name && p.name.toLowerCase().includes(filter));
                }
                if (match && typeFilter) {
                    const hasType = p.apis && p.apis.some(a => a.api_type === typeFilter);
                    if (!hasType) match = false;
                }
                if (match) filtered[name] = p;
            }
            return filtered;
        },

        filteredRoutes() {
            const filter = this.routeFilter.toLowerCase();
            if (!filter) return this.routes;
            const filtered = {};
            for (const [id, r] of Object.entries(this.routes)) {
                if (id.toLowerCase().includes(filter)) {
                    filtered[id] = r;
                    continue;
                }
                if (r.targets) {
                    for (const t of r.targets) {
                        if (t.models) {
                            for (const m of t.models) {
                                if (m.match_model && m.match_model.toLowerCase().includes(filter)) {
                                    filtered[id] = r;
                                    break;
                                }
                            }
                        }
                        if (filtered[id]) break;
                    }
                }
            }
            return filtered;
        },

        providerModelNames(provider) {
            if (!provider.models) return [];
            return provider.models.map(m => {
                if (typeof m === 'string') return m;
                return m.model_id || m;
            });
        },

        providerApis(providerName) {
            return (this.configProviders[providerName] || {}).apis || [];
        },

        apiTypeBadgeClass(type) {
            const map = {
                'anthropic': 'bg-purple-100 text-purple-700 dark:bg-purple-900 dark:text-purple-300',
                'openai': 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900 dark:text-emerald-300',
                'gemini': 'bg-blue-100 text-blue-700 dark:bg-blue-900 dark:text-blue-300'
            };
            return map[type] || 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-300';
        },


        logLevelColor(line) {
            if (line.includes('ERROR')) return 'text-red-400';
            if (line.includes('WARN')) return 'text-yellow-400';
            if (line.includes('DEBUG')) return 'text-gray-400';
            return 'text-green-400';
        },

        // Dashboard stats
        get routeCount() {
            return Object.keys(this.routes).length;
        },
        get providerCount() {
            return Object.keys(this.providers).length;
        },
        get modelCount() {
            let count = 0;
            for (const r of Object.values(this.routes)) {
                if (r.targets) {
                    for (const t of r.targets) {
                        if (t.models) count += t.models.length;
                    }
                }
            }
            return count;
        },
        get todayRequestCount() {
            return this.llmTotal;
        },
        get todayErrorCount() {
            return this.llmLogs.filter(l => !!l.error).length;
        },
        get avgDuration() {
            const logs = this.llmLogs.filter(l => l.duration_ms);
            if (!logs.length) return 0;
            return Math.round(logs.reduce((a, b) => a + b.duration_ms, 0) / logs.length);
        },

        get providerTypes() {
            const types = new Set();
            for (const p of Object.values(this.providers)) {
                if (p.apis) {
                    for (const a of p.apis) {
                        if (a.api_type) types.add(a.api_type);
                    }
                }
            }
            return Array.from(types).sort();
        },

        get enabledProviders() {
            const result = {};
            for (const [name, p] of Object.entries(this.providers)) {
                if (p.enable) result[name] = p;
            }
            return result;
        },

        get firstRouteId() {
            const keys = Object.keys(this.routes);
            return keys.length ? keys[0] : '';
        },

        get firstModelForRoute() {
            if (!this.testRouteId || !this.routes[this.testRouteId]) return '';
            const route = this.routes[this.testRouteId];
            if (route.targets) {
                for (const t of route.targets) {
                    if (t.models && t.models.length) {
                        return t.models[0].match_model || '';
                    }
                }
            }
            return '';
        },

        async sendTest() {
            this.testResult = '';
            this.testError = '';
            this.testLoading = true;
            const routeId = this.testRouteId || this.firstRouteId;
            const model = this.testModel || this.firstModelForRoute;
            if (!routeId || !model) {
                this.testError = 'Please select a route and model';
                this.testLoading = false;
                return;
            }
            try {
                const pathPrefix = this.testUseV1 ? '/v1' : '';
                const res = await fetch(`/llm/${routeId}${pathPrefix}/messages`, {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                        'x-api-key': this.apiKey
                    },
                    body: JSON.stringify({
                        model: model,
                        max_tokens: 256,
                        messages: [{ role: 'user', content: this.testPrompt }]
                    })
                });
                const data = await res.json();
                if (res.ok) {
                    const text = data.content?.map(c => c.type === 'text' ? c.text : '').join('') || JSON.stringify(data, null, 2);
                    this.testResult = text;
                } else {
                    this.testError = data.error?.message || `HTTP ${res.status}`;
                }
            } catch (e) {
                this.testError = e.message;
            } finally {
                this.testLoading = false;
            }
        }
    };
}
