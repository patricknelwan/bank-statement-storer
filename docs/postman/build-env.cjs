const fs = require('node:fs');

const [seedPath, outputPath] = process.argv.slice(2);
if (!seedPath || !outputPath || !process.env.WORKER_INGEST_TOKEN) {
  throw new Error('seed path, output path, and WORKER_INGEST_TOKEN are required');
}
const seed = JSON.parse(fs.readFileSync(seedPath, 'utf8'));
const env = JSON.parse(fs.readFileSync('docs/postman/local.postman_environment.json', 'utf8'));
const values = {
  ...seed,
  worker_token: process.env.WORKER_INGEST_TOKEN,
  base_url: process.env.POSTMAN_BASE_URL || 'http://127.0.0.1:18080',
};
for (const item of env.values) {
  if (Object.hasOwn(values, item.key)) item.value = values[item.key];
}
fs.writeFileSync(outputPath, JSON.stringify(env));
