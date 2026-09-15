/// <reference types="vite-plus/client" />

interface ImportMetaEnv {
  readonly VITE_BUILD_COMMIT: string;
  readonly VITE_PROJECT_MAPPING_WORKSPACE?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
