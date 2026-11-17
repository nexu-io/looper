import { expect, it } from 'vitest';
import { renderFileAnswer } from '../src/card/inline-files';

it('keeps Chinese attachment labels full-width beside long prose', () => {
  const result = renderFileAnswer({
    text: '• GUIDE、REPORT 仍可通过桥入口获取。限制与本次来源已写入状态文件，待同步。',
    links: ['验收指南', '检测报告'].map((label, index) => ({
      token: index === 0 ? 'GUIDE' : 'REPORT',
      element: { tag: 'interactive_container', element_id: `file_${index}`,
        elements: [{ tag: 'markdown', content: label }] },
    })),
  });
  expect(result.filter((element) => element.tag === 'interactive_container')).toHaveLength(2);
  expect(result.some((element) => element.tag === 'column_set')).toBe(false);
  expect(result.map((element) => element.content)).not.toContain('•&nbsp;');
});
