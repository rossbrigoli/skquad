/**
 * Browser abstraction (TG-6 §4).
 *
 * `BrowserInstance` is the seam between the service and a real Chromium process.
 * - `PlaywrightBrowserInstance` wraps playwright-core (headless chromium).
 *   Browsers are NOT downloaded at build/CI time: the executable path comes from
 *   env BROWSER_EXECUTABLE (default 'chromium').
 * - `FakeBrowserInstance` is a deterministic in-memory double used by tests.
 *   Fake pages fire registered route handlers on navigation so the
 *   Context.route(doublestar-all) enforcement path is exercised without Chromium.
 */

// ---------- minimal handle types (framework-agnostic) ----------

export interface RouteRequestInfo {
  /** Absolute URL of the request (top-level, subresource, fetch/XHR, WebSocket). */
  url: string;
  /** Playwright resource type: 'document' | 'script' | 'stylesheet' | 'image' | 'fetch' | 'xhr' | 'websocket' | ... */
  resourceType: string;
}

export interface RouteController {
  abort(): Promise<void>;
  continue(): Promise<void>;
}

export type RouteHandler = (req: RouteRequestInfo, ctrl: RouteController) => Promise<void>;

export interface BrowserPageHandle {
  goto(url: string): Promise<{ title: string }>;
  click(selector: string): Promise<void>;
  fill(selector: string, text: string, submit?: boolean): Promise<void>;
  screenshot(opts: { fullPage?: boolean }): Promise<Buffer>;
  /** Visible body text of the page. */
  visibleText(): Promise<string>;
  close(): Promise<void>;
}

export interface BrowserContextHandle {
  /** Register a route handler (used with the doublestar-all pattern for the security core). */
  route(pattern: string, handler: RouteHandler): Promise<void>;
  newPage(): Promise<BrowserPageHandle>;
  close(): Promise<void>;
}

export interface BrowserInstance {
  readonly id: string;
  launch(): Promise<void>;
  newContext(): Promise<BrowserContextHandle>;
  /** Convenience: create a page inside an existing context. */
  newPage(ctx: BrowserContextHandle): Promise<BrowserPageHandle>;
  close(): Promise<void>;
  isAlive(): boolean;
}

// ---------- playwright-core implementation ----------

/* eslint-disable @typescript-eslint/no-explicit-any */
export class PlaywrightBrowserInstance implements BrowserInstance {
  readonly id: string;
  private browser: import('playwright-core').Browser | null = null;
  private readonly executablePath: string;

  constructor(id: string, executablePath?: string) {
    this.id = id;
    this.executablePath = executablePath ?? process.env.BROWSER_EXECUTABLE ?? 'chromium';
  }

  async launch(): Promise<void> {
    const { chromium } = await import('playwright-core');
    this.browser = await chromium.launch({
      headless: true,
      executablePath: this.executablePath,
    });
    this.browser.on('disconnected', () => {
      this.browser = null;
    });
  }

  async newContext(): Promise<BrowserContextHandle> {
    if (!this.browser) throw new Error('browser not launched');
    const ctx = await this.browser.newContext({
      // Session isolation: no persistence (§4).
      storageState: undefined,
    });
    return new PlaywrightContextHandle(ctx);
  }

  async newPage(ctx: BrowserContextHandle): Promise<BrowserPageHandle> {
    return ctx.newPage();
  }

  async close(): Promise<void> {
    if (this.browser) {
      const b = this.browser;
      this.browser = null;
      await b.close().catch(() => undefined);
    }
  }

  isAlive(): boolean {
    return this.browser !== null && this.browser.isConnected();
  }
}

class PlaywrightContextHandle implements BrowserContextHandle {
  constructor(private readonly ctx: import('playwright-core').BrowserContext) {}

  async route(pattern: string, handler: RouteHandler): Promise<void> {
    await this.ctx.route(pattern, async (route: any) => {
      const req = route.request();
      const ctrl: RouteController = {
        abort: () => route.abort().catch(() => undefined),
        continue: () => route.continue().catch(() => undefined),
      };
      await handler({ url: req.url(), resourceType: req.resourceType() }, ctrl);
    });
  }

  async newPage(): Promise<BrowserPageHandle> {
    const page = await this.ctx.newPage();
    return new PlaywrightPageHandle(page);
  }

  async close(): Promise<void> {
    await this.ctx.close().catch(() => undefined);
  }
}

class PlaywrightPageHandle implements BrowserPageHandle {
  constructor(private readonly page: import('playwright-core').Page) {}

  async goto(url: string): Promise<{ title: string }> {
    const resp = await this.page.goto(url, { waitUntil: 'domcontentloaded' });
    const title = (await this.page.title().catch(() => '')) ?? '';
    void resp;
    return { title };
  }

  async click(selector: string): Promise<void> {
    await this.page.click(selector);
  }

  async fill(selector: string, text: string, submit?: boolean): Promise<void> {
    await this.page.fill(selector, text);
    if (submit) {
      await this.page.press(selector, 'Enter');
    }
  }

  async screenshot(opts: { fullPage?: boolean }): Promise<Buffer> {
    const buf = await this.page.screenshot({ fullPage: !!opts.fullPage, type: 'png' });
    return Buffer.from(buf);
  }

  async visibleText(): Promise<string> {
    const t = await this.page.evaluate('document.body.innerText');
    return typeof t === 'string' ? t : String(t ?? '');
  }

  async close(): Promise<void> {
    await this.page.close().catch(() => undefined);
  }
}

// ---------- fake implementation for tests ----------

export class RouteAbortedError extends Error {
  readonly aborted = true;
  constructor(public readonly url: string) {
    super(`navigation aborted by route handler: ${url}`);
    this.name = 'RouteAbortedError';
  }
}

export class FakePage implements BrowserPageHandle {
  closed = false;
  clicks: string[] = [];
  typed: Array<{ selector: string; text: string; submit?: boolean }> = [];
  constructor(
    private readonly ctx: FakeContext,
    public screenshotSize: number = 1024,
    public text: string = 'fake visible page text',
  ) {}

  async goto(url: string): Promise<{ title: string }> {
    // Fire the context's route handlers exactly like Chromium would for a
    // top-level navigation. If any handler aborts, navigation fails.
    await this.ctx.fireRoute({ url, resourceType: 'document' });
    return { title: `Fake: ${url}` };
  }

  async click(selector: string): Promise<void> {
    this.clicks.push(selector);
  }

  async fill(selector: string, text: string, submit?: boolean): Promise<void> {
    this.typed.push({ selector, text, submit });
  }

  async screenshot(opts: { fullPage?: boolean }): Promise<Buffer> {
    // fullPage screenshots are larger than viewport-only ones, like reality.
    const size = opts.fullPage ? this.screenshotSize * 4 : this.screenshotSize;
    return Buffer.alloc(size, 0x51);
  }

  async visibleText(): Promise<string> {
    return this.text;
  }

  async close(): Promise<void> {
    this.closed = true;
  }
}

export class FakeContext implements BrowserContextHandle {
  closed = false;
  pages: FakePage[] = [];
  routeHandlers: RouteHandler[] = [];
  /** How many requests were allowed vs aborted (observable in tests). */
  allowedCount = 0;
  abortedCount = 0;

  constructor(public readonly ownerInstanceId: string) {}

  async route(pattern: string, handler: RouteHandler): Promise<void> {
    if (pattern !== '**/*') throw new Error(`unexpected route pattern: ${pattern}`);
    this.routeHandlers.push(handler);
  }

  async newPage(): Promise<BrowserPageHandle> {
    const p = new FakePage(this);
    this.pages.push(p);
    return p;
  }

  /** Test helper: run all registered route handlers for a synthetic request. */
  async fireRoute(req: RouteRequestInfo): Promise<'aborted' | 'continued'> {
    let result: string = 'continued';
    for (const h of this.routeHandlers) {
      const ctrl: RouteController = {
        abort: async () => {
          result = 'aborted';
          this.abortedCount++;
        },
        continue: async () => {
          this.allowedCount++;
        },
      };
      await h(req, ctrl);
      if (result === 'aborted') break;
    }
    if (this.routeHandlers.length === 0) this.allowedCount++;
    return result as 'aborted' | 'continued';
  }

  async close(): Promise<void> {
    this.closed = true;
  }
}

export interface FakeOptions {
  screenshotSize?: number;
  text?: string;
}

export class FakeBrowserInstance implements BrowserInstance {
  readonly id: string;
  launched = false;
  closed = false;
  contexts: FakeContext[] = [];
  screenshotSize: number;
  text: string;

  constructor(id: string, opts: FakeOptions = {}) {
    this.id = id;
    this.screenshotSize = opts.screenshotSize ?? 1024;
    this.text = opts.text ?? 'fake visible page text';
  }

  async launch(): Promise<void> {
    this.launched = true;
  }

  async newContext(): Promise<BrowserContextHandle> {
    if (!this.launched || this.closed) throw new Error('instance not usable');
    const ctx = new FakeContext(this.id);
    this.contexts.push(ctx);
    return ctx;
  }

  async newPage(ctx: BrowserContextHandle): Promise<BrowserPageHandle> {
    return ctx.newPage();
  }

  async close(): Promise<void> {
    this.closed = true;
  }

  isAlive(): boolean {
    return this.launched && !this.closed;
  }

  /** Test helper: simulate a Chromium crash. */
  crash(): void {
    this.closed = true;
  }
}
