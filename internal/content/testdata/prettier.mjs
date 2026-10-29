// Writes prettier.json: what Prettier, as a Site's CI runs it, makes of the
// content the Go tests format, of the text they measure, and of the numbers
// they write as JavaScript does. Each case is generated here, with a fixed
// seed, and Prettier's own answer recorded.
//
// Run it, from this repository's root, in a checkout that has the Prettier
// the Sites use installed, such as the edity repository, with that
// checkout's Node:
//
//   mise -C <edity> exec -- node "$PWD/internal/content/testdata/prettier.mjs" \
//     > internal/content/testdata/prettier.json
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { emojiStrings, version } from '../prettier-bundle.mjs';

const prettier = await import(
  pathToFileURL(join(process.cwd(), 'node_modules/prettier/index.mjs')).href
);
const width = prettier.util.getStringWidth;

// As `prettier --check .` reads a Site's content file, with the Site
// template's options.
async function format(text) {
  const options = { filepath: 'src/content/page.json', singleQuote: true };
  const formatted = await prettier.format(text, options);
  if ((await prettier.format(formatted, options)) !== formatted) {
    throw new Error(`Prettier does not keep its own output of ${text}`);
  }
  return formatted;
}

// mulberry32, so that every run writes the same cases.
let seed = 20;
function random() {
  seed = (seed + 0x6d2b79f5) | 0;
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
}
const between = (lo, hi) => lo + Math.floor(random() * (hi - lo + 1));
const pick = (list) => list[Math.floor(random() * list.length)];

const emoji = [...emojiStrings].sort();
// What text is made of: ASCII, accents precomposed and combining, East Asian
// wide and fullwidth characters, control characters, what JSON escapes, and
// emoji whole and in pieces.
const pieces = [
  'a', 'b', 'Z', ' ', '7', '-', 'é', 'ñ', 'ü', 'é', 'ä́', '中', '日本', 'Ａ', '한',
  '\u0007', '\u007f', '\u0085', ' ', '️', '︎', '‍', '\u{1f3fb}', '\u{1f1ea}',
  '\u{1f1f8}', '#', '⃣', '©', '™', '❤', '\u{1f9b4}', '\u{1f9d1}', '⚕', '\u{e0067}', '"', '\\',
  '\n', '\t', ' ', '\u{10ffff}', '\u{20000}',
];
const piece = () => (random() < 0.15 ? pick(emoji) : pick(pieces));
const text = (count) => Array.from({ length: count }, piece).join('');
const plainText = (count) =>
  Array.from({ length: count }, () => pick(['a', 'b', 'c', ' ', 'é', 'x'])).join('');

// Widths of every code point alone, as runs of [first, last, width].
const widths = [];
for (let cp = 0; cp <= 0x10ffff; cp++) {
  if (cp >= 0xd800 && cp <= 0xdfff) continue;
  const w = width(String.fromCodePoint(cp));
  const run = widths.at(-1);
  if (run && run[1] === cp - 1 && run[2] === w) run[1] = cp;
  else widths.push([cp, cp, w]);
}

// Text with emoji in it: each emoji between other characters and before a
// piece of the next one, each without its variation selectors, and random
// text.
const texts = new Set();
emoji.forEach((e, n) => {
  const next = [...emoji[(n + 1) % emoji.length]];
  texts.add(`x${e}${next.slice(0, -1).join('')}y`);
  if (e.includes('️')) texts.add(e.replaceAll('️', ''));
});
for (let n = 0; n < 3000; n++) texts.add(text(between(1, 8)));
const measured = [...texts].map((t) => [t, width(t)]);

// Numbers as JSON has them, and as JavaScript writes them.
const numberTexts = [
  '0', '-0', '1', '-1', '0.1', '1.5', '1e21', '1e-7', '1E+21', '123456789012345680000', '1e20',
  '0.000001', '0.0000001', '5e-324', '1.7976931348623157e308', '0.30000000000000004', '1.50',
  '100e-2', '0.10e2', '12500', '19.99', '49.5', '-2.5e-9', '9007199254740993', '1e400',
];
for (let n = 0; n < 300; n++) {
  const x = (random() - 0.3) * 10 ** between(-30, 30);
  numberTexts.push(String(x), x.toExponential(between(0, 6)), x.toFixed(between(0, 4)));
}
const numbers = numberTexts.map((t) => [t, String(Number(t))]);

// Content files, as Prettier formats each from JSON.stringify's text, with a
// line break after every object's brace, as every content file has. The Go
// printer reads only their values, so the formatted text is all a test needs.
const values = [];
// The edity-sandbox home page as Migration wrote it.
values.push({
  brand: { name: 'Fisio Sandbox' },
  hero: {
    title: 'Fisioterapia cerca de ti, sin esperas',
    lead: 'Valoramos tu caso en la primera visita y te proponemos un plan claro, con ejercicios que puedes hacer en casa entre sesiones.',
    cta: 'Reserva tu cita',
  },
  stats: { patients: 12500 },
  visit: { label: 'Primera visita', price: 35 },
  services: [
    'Fisioterapia deportiva',
    'Suelo pélvico',
    'Punción seca',
    'Rehabilitación tras cirugía 🦴',
  ],
  plans: [
    { name: 'Básico', price: 19.99, features: ['Una sesión al mes', 'Seguimiento por WhatsApp'] },
    {
      name: 'Completo',
      price: 49.5,
      features: ['Cuatro sesiones al mes', 'Plan de ejercicios en casa', 'Revisión trimestral 🧑‍⚕️'],
    },
  ],
  contact: { title: '¿Hablamos de tu caso?', call: 'Llámanos' },
});

// Lists of text whose line is a column or two either side of the print
// width, at several depths, with or without a comma after them, and with
// text that Prettier counts otherwise than by its characters.
const kinds = {
  ascii: () => plainText(1),
  wide: () => pick(['中', '日', 'Ａ', '한', 'a']),
  emoji: () => (random() < 0.5 ? pick(emoji) : 'a'),
  escaped: () => pick(['"', '\\', '\n', '\u0007', 'a']),
  combining: () => pick(['é', 'a', '️', 'a‍']),
};
const nest = (depth, key, inner) => {
  let value = inner;
  for (let d = 0; d < depth; d++) value = { [`${key}${d}`]: value };
  return value;
};
for (const [kind, make] of Object.entries(kinds)) {
  for (let depth = 0; depth <= 3; depth++) {
    for (const key of ['k', 'longerKeyName']) {
      for (const last of [true, false]) {
        for (let delta = -2; delta <= 2; delta++) {
          const head = '  '.repeat(depth + 1) + JSON.stringify('list') + ': ';
          const target = 80 - width(head) - (last ? 0 : 1) + delta;
          const items = [];
          for (let attempt = 0; ; attempt++) {
            items.length = 0;
            const count = between(1, 4);
            for (let n = 0; n < count; n++) items.push(Array.from({ length: between(1, 6) }, make).join(''));
            const line = () => `[${items.map((s) => JSON.stringify(s)).join(', ')}]`;
            while (width(line()) < target) items[items.length - 1] += 'x';
            if (width(line()) === target) break;
            if (attempt > 50) throw new Error(`no List of ${kind} text ${target} columns wide`);
          }
          const inner = last ? { list: items } : { list: items, after: 1 };
          values.push(nest(depth, key, inner));
        }
      }
    }
  }
}

// Lists of numbers, which Prettier fills as many to a line as fit.
for (let n = 0; n < 60; n++) {
  const numbers = Array.from({ length: between(1, 40) }, () => {
    switch (between(0, 4)) {
      case 0:
        return between(0, 9);
      case 1:
        return between(-1e6, 1e6);
      case 2:
        return Math.round(random() * 1e4) / 100;
      case 3:
        return (random() - 0.5) * 10 ** between(-12, 25);
      default:
        return between(0, 1e12);
    }
  });
  values.push(nest(between(0, 3), 'numbers', between(0, 1) ? { list: numbers } : { list: numbers, after: 'x' }));
}

// The shapes Prettier breaks on sight, or not.
values.push(
  { matrix: [[1, 2], [3, 4]] },
  { matrix: [['a', 'b'], ['c', 'd']] },
  { notMatrix: [[1], [2]] },
  { notMatrix: [[1, 2], [3]] },
  { mixed: [[1, 2], 'a'] },
  { empties: [[], [], {}] },
  { emptyObjects: [{}, {}] },
  { objects: [{ a: 1 }] },
  { objects: [{ a: 1, b: 2 }, { a: 3, b: 4 }] },
  { deep: [[[{ a: 1 }]]] },
  { deep: [[[1, 2], [3, 4]]] },
  { deepFlat: [[[1], [2]], 'x'] },
  { mixedNumbers: [1, 'a', 2] },
  { oneNumber: [5] },
  { negative: [-1, -2, 3] },
  { emptyText: ['', ''] },
  {},
  { empty: {} },
  { list: [] },
);

// Content of random shapes.
const randomValue = (depth) => {
  switch (between(0, depth > 2 ? 3 : 7)) {
    case 0:
      return text(between(0, 30));
    case 1:
      return between(-1000, 100000) / pick([1, 10, 100]);
    case 2:
      return Array.from({ length: between(0, 8) }, () => text(between(0, 12)));
    case 3:
      return Array.from({ length: between(0, 20) }, () => between(-99, 999));
    case 4:
      return Array.from({ length: between(0, 3) }, () => randomObject(depth + 1));
    case 5:
      return Array.from({ length: between(0, 3) }, () =>
        Array.from({ length: between(0, 3) }, () => text(between(0, 4))),
      );
    default:
      return randomObject(depth + 1);
  }
};
const randomObject = (depth) =>
  Object.fromEntries(Array.from({ length: between(0, 5) }, () => [text(between(1, 6)), randomValue(depth)]));
for (let n = 0; n < 150; n++) values.push(randomObject(0));

const files = [];
for (const value of values) files.push(await format(JSON.stringify(value, null, 2)));
// Numbers as a Developer may have written them, which Prettier rewrites.
const written = `{
  "numbers": [1E5, 1.50, 1e+05, 1e-007, 1.0, 1.000, -0.0, 0.10e2, 1e0, 2E-0, 5, 10.0500, 1e00, 3.0e+00]
}`;

// One case per line, so that a diff after a Prettier update stays readable.
const lines = (cases) => cases.map((c) => `    ${JSON.stringify(c)}`).join(',\n');
process.stdout.write(`{
  "prettier": ${JSON.stringify(version)},
  "widths": [
${lines(widths)}
  ],
  "texts": [
${lines(measured)}
  ],
  "numbers": [
${lines(numbers)}
  ],
  "files": [
${lines(files)}
  ],
  "written": ${JSON.stringify([written, await format(written)])}
}
`);
