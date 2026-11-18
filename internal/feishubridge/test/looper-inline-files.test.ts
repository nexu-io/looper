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

it('keeps an internal skill reference from splitting Chinese prose into columns', () => {
  const result = renderFileAnswer({
    text: '已补齐预览 HTML，保留首页和社区模板入口，通过 SKILL 提交 [PR #9530](https://github.com/nexu-io/open-design/pull/9530)。',
    links: [{ token: 'SKILL', element: { tag: 'interactive_container',
      elements: [{ tag: 'markdown', content: 'Odcrew 技能' }] } }],
  });
  expect(result.some((element) => element.tag === 'column_set')).toBe(false);
  expect(result.filter((element) => element.tag === 'interactive_container')).toHaveLength(1);
  expect(JSON.stringify(result)).toContain('https://github.com/nexu-io/open-design/pull/9530');
});
