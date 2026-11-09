# -*- coding: utf-8 -*-
import sys, json
sys.stdout.reconfigure(encoding='utf-8')
from openpyxl import load_workbook, Workbook
from openpyxl.styles import Font, PatternFill, Alignment, Border, Side
from collections import defaultdict

def norm(t):
    return str(t).strip().upper()

def _tofloat(v):
    try:
        return float(v)
    except Exception:
        return 0.0

# ---------- 1. 惠众账单 ----------
H = json.load(open('huizhong_parsed.json', encoding='utf-8'))
# 单价：10月明细C = 34元/项，10月明细A = 26元/项（票税另计，见下）
PRICE = {'C': 34, 'A': 26}
oct_rows = []
for tag, rows in (('A', H['octA']), ('C', H['octC'])):
    for x in rows:
        cats = []
        if x['cinv']: cats.append('票')
        if x['ctax']: cats.append('税')
        oct_rows.append(dict(tax=norm(x['tax']), name=x['name'], tm=x['tm'],
                             cats=cats, block=tag, price=PRICE[tag]))

# 惠众9月明细
hz_sep = defaultdict(set)
hz_sep_times = defaultdict(list)
for x in H['sep']:
    if x['cinv']: hz_sep[norm(x['tax'])].add('票')
    if x['ctax']: hz_sep[norm(x['tax'])].add('税')
    hz_sep_times[norm(x['tax'])].append(x['tm'])

# 惠众10月：按税号聚合收费类目
hz_oct_cats = defaultdict(set)
hz_oct_name = {}
hz_oct_times = defaultdict(list)
hz_oct_price = {}
for r in oct_rows:
    hz_oct_cats[r['tax']].update(r['cats'])
    if r['name']:
        hz_oct_name[r['tax']] = r['name']
    hz_oct_times[r['tax']].append(r['tm'])
    hz_oct_price[r['tax']] = r['price']
hz_oct_ids = set(hz_oct_cats)

# ---------- 2. 联信数据 ----------
# 2a. 原始账单：请求明细（时间+维度），用于取9月调用记录和全量请求集合
lx_bill = r'c:\Users\XMMXYY\Downloads\下游账单-北京联信征信咨询有限责任公司-2026-09-01至2026-10-31.xlsx'
wb = load_workbook(lx_bill, read_only=True, data_only=True)
DIM2CATS = {'invoice': ['票'], 'tax': ['税'], 'invoice+tax': ['票', '税'], 'both': ['票', '税']}
lx_requested = set()           # 联信9-10月请求过的所有税号
lx_sep_cats = defaultdict(set) # 联信9月调用的类目
lx_name = {}
for sheet in ['票明细', '税明细', '票税明细']:
    ws = wb[sheet]
    rows = ws.iter_rows(values_only=True); hdr = list(next(rows))
    ix = {c: i for i, c in enumerate(hdr)}
    for r in rows:
        if not r or not r[ix['税号']]:
            continue
        tax = norm(r[ix['税号']])
        lx_requested.add(tax)
        dim = str(r[ix['请求维度']] or '').strip()
        tm = str(r[ix['时间']] or '')
        nm = r[ix['企业名称']]
        if nm:
            lx_name[tax] = nm
        if tm.startswith('2026-09'):
            lx_sep_cats[tax].update(DIM2CATS.get(dim, []))

# 2b. 重调金额数据（用于Q3），仅覆盖联信538
recall = [json.loads(l) for l in open('lianxin_recall/progress.jsonl', encoding='utf-8') if l.strip()]
amt = {}
for r in recall:
    amt[norm(r['credit'])] = r
    if r.get('company'):
        lx_name.setdefault(norm(r['credit']), r['company'])

def cname(tax):
    return hz_oct_name.get(tax) or lx_name.get(tax) or ''

# ---------- Q1：9月已调(联信) + 10月又收费(惠众)，同类目重复 ----------
q1 = []
for tax in sorted(hz_oct_ids):
    sep = lx_sep_cats.get(tax)
    if not sep:
        continue
    octc = hz_oct_cats[tax]
    dup = sep & octc          # 同类目 => 应在免费期，重复收费
    newc = octc - sep         # 10月新类目 => 可正常收费
    if dup:
        price = hz_oct_price[tax]
        refund = len(dup) * price
        q1.append(dict(tax=tax, name=cname(tax),
                       sep='、'.join(sorted(lx_sep_cats[tax])),
                       oct='、'.join(sorted(octc)),
                       dup='、'.join(sorted(dup)),
                       newc='、'.join(sorted(newc)) or '无',
                       price=price, refund=refund))

# ---------- Q1补充：惠众账单自身 9月已收 + 10月同类目又收 ----------
q1b = []
for tax in sorted(hz_oct_ids & set(hz_sep)):
    dup = hz_sep[tax] & hz_oct_cats[tax]
    if dup:
        price = hz_oct_price[tax]
        q1b.append(dict(tax=tax, name=cname(tax),
                        sep='、'.join(sorted(hz_sep[tax])),
                        oct='、'.join(sorted(hz_oct_cats[tax])),
                        dup='、'.join(sorted(dup)),
                        septime='；'.join(sorted(hz_sep_times[tax])),
                        octtime='；'.join(sorted(hz_oct_times[tax])),
                        price=price, refund=len(dup) * price))

# ---------- Q2：惠众10月收费、但联信未请求过的税号 ----------
q2 = []
for tax in sorted(hz_oct_ids):
    if tax in lx_requested:
        continue
    q2.append(dict(tax=tax, name=cname(tax),
                   oct='、'.join(sorted(hz_oct_cats[tax])),
                   times='；'.join(sorted(hz_oct_times[tax])),
                   cnt=len(hz_oct_times[tax])))

# ---------- Q3：惠众10月收费的税号中，所收类目12个月金额为0（仅联信重合151个有金额数据） ----------
q3 = []
for tax in sorted(hz_oct_ids & lx_requested):
    a = amt.get(tax)
    if not a:
        continue
    octc = hz_oct_cats[tax]
    zero_cats = []
    detail = []
    if '票' in octc:
        iv = a.get('invAmt'); iz = a.get('invZero')
        detail.append(f"票12月={iv}")
        if iz is True or (iv is not None and _tofloat(iv) <= 0):
            zero_cats.append('票')
    if '税' in octc:
        tv = a.get('taxAmt'); tz = a.get('taxZero')
        detail.append(f"税12月={tv}")
        if tz is True or (tv is not None and _tofloat(tv) <= 0):
            zero_cats.append('税')
    if zero_cats:
        q3.append(dict(tax=tax, name=cname(tax),
                       oct='、'.join(sorted(octc)),
                       zero='、'.join(zero_cats),
                       detail='；'.join(detail),
                       total=a.get('total')))

# ---------- 输出 Excel ----------
print('惠众10月计费税号', len(hz_oct_ids), '| 联信请求过', len(lx_requested))
print('Q1 (联信9月)同类目重复', len(q1))
print('Q1补充 (惠众自身)同类目重复', len(q1b), '预估应退', sum(x['refund'] for x in q1b))
print('Q2 联信未调用税号', len(q2))
print('Q3 金额为0税号', len(q3))

TITLE = Font(bold=True, size=13)
HEAD = Font(bold=True, color='FFFFFF')
HFILL = PatternFill('solid', fgColor='4F6228')
WARN = PatternFill('solid', fgColor='FCE4D6')
thin = Side(style='thin', color='BFBFBF')
BORD = Border(left=thin, right=thin, top=thin, bottom=thin)
WRAP = Alignment(wrap_text=True, vertical='top')

wb_out = Workbook()

def style_header(ws, ncol):
    for c in range(1, ncol + 1):
        cell = ws.cell(row=1, column=c)
        cell.font = HEAD
        cell.fill = HFILL
        cell.border = BORD
    ws.freeze_panes = 'A2'

def widths(ws, ws_widths):
    for col, w in ws_widths.items():
        ws.column_dimensions[col].width = w

# 汇总
ws = wb_out.active
ws.title = '汇总'
ws['A1'] = '惠众10月账单 × 联信手工数据 对账汇总'
ws['A1'].font = TITLE
info = [
    ('惠众客户', '冠辰科技（appId 9VTYC6YU）'),
    ('对账基准', '北京联信征信咨询有限责任公司手工整理数据'),
    ('惠众10月计费税号总数', len(hz_oct_ids)),
    ('惠众10月计费明细行数', len(oct_rows)),
    ('联信9-10月请求税号总数', len(lx_requested)),
    ('', ''),
    ('问题1：联信9月已调、惠众10月同类目又收费', f'{len(q1)} 个税号（联信9月调用与惠众10月账单无重合）'),
    ('问题1补充：惠众账单自身9月已收、10月同类目又收', f'{len(q1b)} 个税号，预估应退 {sum(x["refund"] for x in q1b)} 元'),
    ('问题2：联信从未请求过、惠众却计费', f'{len(q2)} 个税号'),
    ('问题3：所收类目近12个月金额为0（仅联信重合151个有金额）', f'{len(q3)} 个税号'),
    ('', ''),
    ('说明1：问题1按你的选择以联信9月调用记录为准。联信9月共调131个税号，上游走“证通”，与惠众10月账单零重合，故为0。', ''),
    ('说明2：惠众10月账单与联信全量请求仅重合151个税号；其余335个联信未请求，见问题2，按要求不计算金额。', ''),
    ('说明3：问题3金额来自联信税号重调结果，按惠众实际所收类目判断近12个月是否<=0。', ''),
]
r = 3
for k, v in info:
    ws.cell(row=r, column=1, value=k)
    ws.cell(row=r, column=2, value=v)
    r += 1
ws.column_dimensions['A'].width = 52
ws.column_dimensions['B'].width = 60

# Q1
ws = wb_out.create_sheet('1_9月已调10月又收费')
cols = ['税号', '企业名称', '联信9月调用类目', '惠众10月收费类目', '重复收费类目(应免费)', '10月新增可计费类目', '单价', '预估应退(元)']
ws.append(cols)
for x in q1:
    ws.append([x['tax'], x['name'], x['sep'], x['oct'], x['dup'], x['newc'], x['price'], x['refund']])
style_header(ws, len(cols))
widths(ws, {'A': 22, 'B': 30, 'C': 16, 'D': 16, 'E': 18, 'F': 18, 'G': 8, 'H': 12})
for row in ws.iter_rows(min_row=2):
    for cell in row:
        cell.border = BORD
        cell.fill = WARN

# Q1 补充：惠众自身重复
ws = wb_out.create_sheet('1补_惠众自身9月已收又收')
cols = ['税号', '企业名称', '惠众9月收费类目', '惠众10月收费类目', '重复收费类目(应免费)', '9月调用时间', '10月调用时间', '单价', '预估应退(元)']
ws.append(cols)
for x in q1b:
    ws.append([x['tax'], x['name'], x['sep'], x['oct'], x['dup'], x['septime'], x['octtime'], x['price'], x['refund']])
style_header(ws, len(cols))
widths(ws, {'A': 22, 'B': 30, 'C': 14, 'D': 14, 'E': 18, 'F': 22, 'G': 22, 'H': 8, 'I': 12})
for row in ws.iter_rows(min_row=2):
    for cell in row:
        cell.border = BORD
        cell.fill = WARN

# Q2
ws = wb_out.create_sheet('2_联信未调用')
cols = ['税号', '企业名称', '惠众10月收费类目', '惠众调用次数', '惠众10月调用时间']
ws.append(cols)
for x in q2:
    ws.append([x['tax'], x['name'], x['oct'], x['cnt'], x['times']])
style_header(ws, len(cols))
widths(ws, {'A': 22, 'B': 30, 'C': 16, 'D': 12, 'E': 60})
for row in ws.iter_rows(min_row=2):
    row[4].alignment = WRAP
    for cell in row:
        cell.border = BORD

# Q3
ws = wb_out.create_sheet('3_金额为0')
cols = ['税号', '企业名称', '惠众10月收费类目', '金额为0的类目', '近12月金额明细', '近12月合计']
ws.append(cols)
for x in q3:
    ws.append([x['tax'], x['name'], x['oct'], x['zero'], x['detail'], x['total']])
style_header(ws, len(cols))
widths(ws, {'A': 22, 'B': 30, 'C': 16, 'D': 14, 'E': 28, 'F': 14})
for row in ws.iter_rows(min_row=2):
    for cell in row:
        cell.border = BORD
        cell.fill = WARN

# 附：惠众10月全量（带标记）
ws = wb_out.create_sheet('附_惠众10月全量')
cols = ['税号', '企业名称', '惠众10月收费类目', '联信是否请求过', '联信9月调用类目', '问题标记']
ws.append(cols)
q1set = {x['tax'] for x in q1} | {x['tax'] for x in q1b}
q3set = {x['tax'] for x in q3}
for tax in sorted(hz_oct_ids):
    flags = []
    if tax in q1set: flags.append('重复收费')
    if tax not in lx_requested: flags.append('联信未调用')
    if tax in q3set: flags.append('金额为0')
    ws.append([tax, cname(tax), '、'.join(sorted(hz_oct_cats[tax])),
               '是' if tax in lx_requested else '否',
               '、'.join(sorted(lx_sep_cats.get(tax, []))) or '',
               '、'.join(flags) or '正常'])
style_header(ws, len(cols))
widths(ws, {'A': 22, 'B': 30, 'C': 16, 'D': 14, 'E': 16, 'F': 24})
for row in ws.iter_rows(min_row=2):
    for cell in row:
        cell.border = BORD

out = r'c:\Users\XMMXYY\Downloads\惠众10月对账-对比联信手工数据.xlsx'
wb_out.save(out)
print('saved', out)
