// Writes core-violations.json: what @edity/core's violations() finds in each
// Draft of the page fixtures, which the Editor checks a Draft with. The Go
// tests check that this package names the same violations. It reads core
// and the fixtures from the edity checkout it is given, which has to be at
// the contracts tag go.mod pins and built with `mise run build`. Run it with
//
//   mise run record:core-violations <edity>
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const edity = process.argv[2];
if (!edity) {
  throw new Error('Give the path of an edity checkout built with `mise run build`.');
}
const { defineFields, definitionOf, list, number, text, violations } =
  await import(pathToFileURL(join(edity, 'packages/core/dist/index.js')).href);

// The Field definitions a page fixture's schema was generated from, read back
// as packages/core/src/contract.test.ts does, which checks the reading.
function shapeOf(schema) {
  return Object.fromEntries(
    schema.required.map((key) => [key, nodeOf(schema.properties[key])]),
  );
}

function nodeOf(schema) {
  switch (schema.type) {
    case 'string':
      return text({
        maxChars: schema['x-edity-maxGraphemes'],
        maxLines: schema['x-edity-maxLines'],
        maxLinesNarrow: schema['x-edity-maxLinesNarrow'],
      });
    case 'number':
    case 'integer':
      return number({
        min: schema.minimum,
        max: schema.maximum,
        decimals: schema.type === 'integer' ? 0 : schema['x-edity-decimals'],
      });
    case 'array':
      return list(nodeOf(schema.items), {
        min: schema.minItems,
        max: schema.maxItems,
      });
    case 'object':
      return shapeOf(schema);
    default:
      throw new TypeError(`Not a page schema: "type": ${schema.type}`);
  }
}

const pages = join(edity, 'contracts/fixtures/pages');
const out = {};
for (const name of readdirSync(pages).filter((n) => n.endsWith('.json')).sort()) {
  const file = (out[`fixtures/pages/${name}`] = {});
  for (const c of JSON.parse(readFileSync(join(pages, name), 'utf8'))) {
    const definition = definitionOf(defineFields(shapeOf(c.schema)));
    const tests = (file[c.description] = {});
    for (const t of c.tests) {
      tests[t.description] = violations(definition, t.data).map(
        ({ path, rule }) => ({ path, limit: rule }),
      );
    }
  }
}
writeFileSync(new URL('core-violations.json', import.meta.url), `${JSON.stringify(out, null, 2)}\n`);
