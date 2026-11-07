const fs = require('node:fs');
const config = JSON.parse(fs.readFileSync(require('node:path').join(__dirname, '../measure-go-cache.json'), 'utf8'));
const group = process.env.GROUP;
const measured = ['race-linux', 'test-Linux-X64'].includes(group);
const env = { MEASURE_GO_CACHE: String(measured), MEASURE_GO_CACHE_SAVE: 'false', MEASURE_GO_CACHE_TRACK: 'false', MEASURE_GO_CACHE_SAVE_KEY: '', GO_CACHE_TRACKING: '' };
if (measured) {
  if (!['baseline', 'g1', 'g2', 'read'].includes(config.generation)) throw new Error('Choose baseline, g1, g2, or read');
  const source = config.sources[group];
  const prefix = ['baseline', 'g1'].includes(config.generation) ? `go-${group}-` : `measure-go-cache-${group}-${config.generation === 'g2' ? 'g1' : 'g2'}-`;
  if (typeof source !== 'string' || !source.startsWith(prefix)) throw new Error(`Choose an exact ${prefix} source key`);
  env.GO_CACHE_KEY = source;
  env.GO_CACHE_PREFIX = '';
  env.GO_CACHE_GROUP_PREFIX = '';
  env.MEASURE_GO_CACHE_GENERATION = config.generation;
  env.MEASURE_GO_CACHE_SAVE = String(['g1', 'g2'].includes(config.generation) && process.env.OWNER === 'true');
  if (env.MEASURE_GO_CACHE_SAVE === 'true') env.MEASURE_GO_CACHE_SAVE_KEY = `measure-go-cache-${group}-${config.generation}-${process.env.GITHUB_RUN_ID}`;
}
env.MEASURE_GO_CACHE_TRACK = String(env.MEASURE_GO_CACHE_SAVE === 'true' || (config.generation === 'g1' && process.env.OWNER === 'true' && ['Windows', 'macOS'].includes(process.env.RUNNER_OS)));
fs.appendFileSync(process.env.GITHUB_ENV, Object.entries(env).map(([key, value]) => `${key}=${value}\n`).join(''));
console.log(JSON.stringify({ group, ...env }));
