// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { ui } from "../../stores/ui.svelte.js";
// @ts-ignore
import PublishModal from "./PublishModal.svelte";

const services = vi.hoisted(() => {
  type Config = { configured: boolean };
  const state = {
    resolveConfig: (_config: Config) => {},
    configPromise: Promise.resolve({ configured: true }),
    configureGeneratedClient: vi.fn(),
    getApiV1ConfigGithub: vi.fn(),
    postApiV1ConfigGithub: vi.fn(),
    postApiV1InsightsIdPublish: vi.fn(),
    postApiV1SessionsIdPublish: vi.fn(),
  };

  function deferConfig() {
    state.configPromise = new Promise<Config>((resolve) => {
      state.resolveConfig = resolve;
    });
    state.getApiV1ConfigGithub.mockImplementation(
      () => state.configPromise,
    );
  }

  return {
    configureGeneratedClient: state.configureGeneratedClient,
    getApiV1ConfigGithub: state.getApiV1ConfigGithub,
    postApiV1ConfigGithub: state.postApiV1ConfigGithub,
    postApiV1InsightsIdPublish:
      state.postApiV1InsightsIdPublish,
    postApiV1SessionsIdPublish:
      state.postApiV1SessionsIdPublish,
    resolveConfig(config: Config) {
      state.resolveConfig(config);
    },
    deferConfig,
  };
});

const sessionState = vi.hoisted(() => ({
  sessions: {
    activeSessionId: "session-123",
  },
}));

vi.mock("../../api/runtime.js", () => ({
  configureGeneratedClient: services.configureGeneratedClient,
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  isAbortError: vi.fn(() => false),
}));

vi.mock("../../api/generated/index", () => ({
  ConfigService: {
    getApiV1ConfigGithub: services.getApiV1ConfigGithub,
    postApiV1ConfigGithub: services.postApiV1ConfigGithub,
  },
  InsightsService: {
    postApiV1InsightsIdPublish:
      services.postApiV1InsightsIdPublish,
  },
  SessionsService: {
    postApiV1SessionsIdPublish:
      services.postApiV1SessionsIdPublish,
  },
}));

vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: sessionState.sessions,
}));

async function flushAsync() {
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

describe("PublishModal", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    services.configureGeneratedClient.mockClear();
    services.getApiV1ConfigGithub.mockClear();
    services.postApiV1ConfigGithub.mockReset();
    services.postApiV1ConfigGithub.mockResolvedValue({});
    services.postApiV1InsightsIdPublish.mockReset();
    services.postApiV1InsightsIdPublish.mockResolvedValue({
      gist_url: "https://gist.github.com/insight",
      view_url: "https://gist.github.com/insight/view",
    });
    services.postApiV1SessionsIdPublish.mockReset();
    services.postApiV1SessionsIdPublish.mockResolvedValue({
      gist_url: "https://gist.github.com/session",
      view_url: "https://gist.github.com/session/view",
    });
    services.deferConfig();
    sessionState.sessions.activeSessionId = "session-123";
    ui.activeModal = null;
    ui.clearPublishTarget();
    ui.publishSecret = false;
    document.body.innerHTML = "";
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    ui.activeModal = null;
    ui.clearPublishTarget();
    ui.publishSecret = false;
    document.body.innerHTML = "";
  });

  it("does not publish the active session after an insight publish closes during setup", async () => {
    ui.setPublishTarget({ kind: "insight", id: 42 });
    ui.publishSecret = true;
    ui.activeModal = "publish";

    component = mount(PublishModal, { target: document.body });
    await tick();
    expect(services.getApiV1ConfigGithub).toHaveBeenCalledTimes(1);

    ui.activeModal = null;
    await tick();
    unmount(component);
    component = undefined;

    services.resolveConfig({ configured: true });
    await flushAsync();

    expect(
      services.postApiV1InsightsIdPublish,
    ).not.toHaveBeenCalled();
    expect(
      services.postApiV1SessionsIdPublish,
    ).not.toHaveBeenCalled();
  });
});
