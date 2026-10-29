// Reads, from the bundle of the Prettier installed in the current directory,
// what Prettier measures a line's width with (getStringWidth in its
// src/utilities/get-string-width.js), which decides where it breaks a line
// of a content file. It fails if getStringWidth is not the one width.go
// ports. emoji.mjs writes emoji.go from it, and testdata/prettier.mjs tests
// width.go with the emoji it matches.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const prettier = join(process.cwd(), 'node_modules/prettier');
export const { version } = JSON.parse(readFileSync(join(prettier, 'package.json'), 'utf8'));
const bundle = readFileSync(join(prettier, 'index.mjs'), 'utf8');

function extract(pattern, what) {
  const found = bundle.match(pattern);
  if (!found) throw new Error(`Prettier ${version}: ${what} not found in its bundle`);
  return found[1];
}

// The function width.go ports, as Prettier 3.9 bundles it.
const portedFunction = `function getStringWidth(text) {
  if (!text) {
    return 0;
  }
  if (!notAsciiRegex.test(text)) {
    return text.length;
  }
  let width = 0;
  text = text.replace(emoji_regex_default(), (character) => {
    width += isNarrowEmojiCharacter(character) ? 1 : 2;
    return "";
  });
  for (const character of text) {
    const codePoint = character.codePointAt(0);
    if (codePoint <= 31 || codePoint >= 127 && codePoint <= 159) {
      continue;
    }
    if (codePoint >= 768 && codePoint <= 879) {
      continue;
    }
    if (codePoint >= 65024 && codePoint <= 65039) {
      continue;
    }
    width += isFullWidth(codePoint) || isWide(codePoint) ? 2 : 1;
  }
  return width;
}`;
const bundled = extract(/(function getStringWidth\(text\) \{[\s\S]*?\n\})/, 'getStringWidth');
if (bundled !== portedFunction) {
  throw new Error(`Prettier ${version}: getStringWidth is not the one width.go ports:\n${bundled}`);
}
if (extract(/var notAsciiRegex = (\/.*\/);/, 'notAsciiRegex') !== '/[^\\x20-\\x7F]/') {
  throw new Error(`Prettier ${version}: notAsciiRegex is not the one width.go ports`);
}

// A parser for what these regular expressions use: \u and \x escapes,
// literals, classes with ranges, non-capturing groups, alternation and ?.
function parse(source) {
  let i = 0;
  const fail = (why) => {
    throw new Error(`${why} at ${i} of ${source.slice(i, i + 20)}`);
  };
  const char = (inClass) => {
    if (source[i] === '\\') {
      const digits = { u: 4, x: 2 }[source[i + 1]] ?? fail('unknown escape');
      const hex = source.slice(i + 2, i + 2 + digits);
      if (!/^[0-9A-Fa-f]+$/.test(hex) || hex.length !== digits) fail('bad escape');
      i += 2 + digits;
      return parseInt(hex, 16);
    }
    if ((inClass ? '[]' : '()[]{}|?*+.^$').includes(source[i])) fail('unsupported syntax');
    return source.charCodeAt(i++);
  };
  const atom = () => {
    if (source.startsWith('(?:', i)) {
      i += 3;
      const group = alternatives();
      if (source[i++] !== ')') fail('unclosed group');
      return { group };
    }
    if (source[i] === '[') {
      i++;
      if (source[i] === '^') fail('negated class');
      const ranges = [];
      while (source[i] !== ']') {
        if (i >= source.length) fail('unclosed class');
        const low = char(true);
        let high = low;
        if (source[i] === '-' && source[i + 1] !== ']') {
          i++;
          high = char(true);
        }
        ranges.push([low, high]);
      }
      i++;
      return { ranges };
    }
    return { char: char(false) };
  };
  const sequence = () => {
    const terms = [];
    while (i < source.length && source[i] !== '|' && source[i] !== ')') {
      const term = { atom: atom(), optional: false };
      if (source[i] === '?') {
        term.optional = true;
        i++;
      }
      terms.push(term);
    }
    return terms;
  };
  const alternatives = () => {
    const sequences = [sequence()];
    while (source[i] === '|') {
      i++;
      sequences.push(sequence());
    }
    return sequences;
  };
  const parsed = alternatives();
  if (i !== source.length) fail('unparsed');
  return parsed;
}

// JavaScript matches these UTF-16 code units, Go matches code points: joins
// each high surrogate with the low surrogate after it. Where a pair spans a
// group's edge, the terms after the group move into each of its
// alternatives, and the surrogate before it too: X(?:a|b)?Y becomes
// X(?:aY|bY|Y). That keeps the order JavaScript tries them in, so Go's
// leftmost-first matching finds what JavaScript finds.
const isHigh = (unit) => unit >= 0xd800 && unit <= 0xdbff;
const isLow = (unit) => unit >= 0xdc00 && unit <= 0xdfff;
const join16 = (high, low) => 0x10000 + ((high - 0xd800) << 10) + (low - 0xdc00);

function toCodePoints(sequences) {
  return sequences.map(codePointSequence);
}

// Whether a sequence can end in a high surrogate, which pairs with what
// follows it.
function endsInHigh(terms) {
  const last = terms.at(-1);
  if (last === undefined) return false;
  if (last.atom.char !== undefined) return isHigh(last.atom.char);
  return last.atom.group !== undefined && last.atom.group.some(endsInHigh);
}

// The group's alternatives, each followed by rest, and rest alone last when
// the group is optional.
function distribute(group, optional, before, rest) {
  const sequences = group.map((seq) => [...before, ...seq, ...rest]);
  if (optional) sequences.push([...before, ...rest]);
  return { atom: { group: toCodePoints(sequences) }, optional: false };
}

function codePointSequence(terms) {
  const out = [];
  for (let k = 0; k < terms.length; k++) {
    const term = terms[k];
    const { atom, optional } = term;
    if (atom.char !== undefined && isHigh(atom.char)) {
      const next = terms[k + 1];
      if (optional || next === undefined) throw new Error('a lone high surrogate');
      if (next.atom.group !== undefined) {
        out.push(distribute(next.atom.group, next.optional, [term], terms.slice(k + 2)));
        return out;
      }
      if (next.optional) throw new Error('a high surrogate before an optional one');
      k++;
      if (next.atom.char !== undefined) {
        if (!isLow(next.atom.char)) throw new Error('a high surrogate before a non-surrogate');
        out.push({ atom: { char: join16(atom.char, next.atom.char) }, optional: false });
      } else {
        if (!next.atom.ranges.every(([lo, hi]) => isLow(lo) && isLow(hi))) {
          throw new Error('a high surrogate before a class of non-surrogates');
        }
        out.push({
          atom: { ranges: next.atom.ranges.map(([lo, hi]) => [join16(atom.char, lo), join16(atom.char, hi)]) },
          optional: false,
        });
      }
      continue;
    }
    if (atom.char !== undefined && isLow(atom.char)) throw new Error('a lone low surrogate');
    if (atom.ranges && atom.ranges.some(([lo, hi]) => lo <= 0xdfff && hi >= 0xd800)) {
      throw new Error('a class of surrogates');
    }
    if (atom.group !== undefined && atom.group.some(endsInHigh)) {
      out.push(distribute(atom.group, optional, [], terms.slice(k + 1)));
      return out;
    }
    if (atom.group !== undefined) {
      out.push({ atom: { group: toCodePoints(atom.group) }, optional });
    } else {
      out.push(term);
    }
  }
  return out;
}

// Every string a pattern matches, which is finite with only ? as a
// quantifier.
function language(sequences, unit) {
  const strings = new Set();
  for (const terms of sequences) {
    let partial = [''];
    for (const { atom, optional } of terms) {
      let choices;
      if (atom.char !== undefined) choices = [unit(atom.char)];
      else if (atom.ranges) {
        choices = atom.ranges.flatMap(([lo, hi]) =>
          Array.from({ length: hi - lo + 1 }, (_, n) => unit(lo + n)),
        );
      } else choices = [...language(atom.group, unit)];
      if (optional) choices = ['', ...choices];
      partial = partial.flatMap((p) => choices.map((c) => p + c));
    }
    for (const p of partial) strings.add(p);
  }
  return strings;
}

const emoji = parse(extract(/var emoji_regex_default = \(\) => \{\s*return \/(.*)\/g;\s*\};/, 'emoji-regex'));
// emoji-regex's pattern, in code points.
export const emojiCodePoints = toCodePoints(emoji);
// Every emoji sequence it matches.
export const emojiStrings = language(emojiCodePoints, (c) => String.fromCodePoint(c));
// The code points an emoji sequence starts with, in order.
export const emojiStarts = [...new Set([...emojiStrings].map((s) => s.codePointAt(0)))].sort((a, b) => a - b);
const fromUnits = language(emoji, (u) => String.fromCharCode(u));
if (fromUnits.size !== emojiStrings.size || [...fromUnits].some((s) => !emojiStrings.has(s))) {
  throw new Error('joining surrogates changed what the emoji pattern matches');
}

const narrow = parse(extract(/var narrowEmojiRegexp = \/\^\(\?:(.*)\)\$\/;/, 'narrowEmojiRegexp'));
// The narrow emoji, in order.
export const narrowEmoji = [...language(toCodePoints(narrow), (c) => String.fromCodePoint(c))].map((s) => {
  const codePoints = [...s];
  if (codePoints.length !== 1) throw new Error(`a narrow emoji of ${codePoints.length} code points`);
  return codePoints[0].codePointAt(0);
});
narrowEmoji.sort((a, b) => a - b);

function ranges(name) {
  const values = JSON.parse(extract(new RegExp(`var ${name} = (\\[[^\\]]*\\]);`), name));
  if (values.length % 2 !== 0 || values.some((v, n) => n > 0 && v < values[n - 1])) {
    throw new Error(`${name} is not a sorted list of ranges`);
  }
  return values;
}

// get-east-asian-width's ranges of Fullwidth and Wide code points.
export const fullwidthRanges = ranges('fullwidthRanges');
export const wideRanges = ranges('wideRanges');
