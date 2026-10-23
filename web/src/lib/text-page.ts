export const TEXT_PAGE_SIZE = 16_000;

// Bound DOM text/layout without truncating the stored reasoning. UTF-16 page
// boundaries must not split an emoji/supplementary character into replacement glyphs.
export function textPage(text: string, page: number, size = TEXT_PAGE_SIZE) {
  const count = Math.max(1, Math.ceil(text.length / size));
  const index = Math.min(count - 1, Math.max(0, page));
  const boundary = (offset: number) => {
    const code = text.charCodeAt(offset);
    return offset > 0 && code >= 0xdc00 && code <= 0xdfff &&
      text.charCodeAt(offset - 1) >= 0xd800 && text.charCodeAt(offset - 1) <= 0xdbff ? offset - 1 : offset;
  };
  return { count, index, text: text.slice(boundary(index * size), boundary(Math.min(text.length, (index + 1) * size))) };
}
