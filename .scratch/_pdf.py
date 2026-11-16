import fitz, sys
sys.stdout.reconfigure(encoding="utf-8")
d=fitz.open(r"c:\workspace\DataHub_SWFP\docs\API_接口文档与使用手册_swfp.pdf")
out=[]
for i,pg in enumerate(d):
    out.append(f"\n===== PAGE {i+1} =====\n"+pg.get_text())
txt="\n".join(out)
open("_apidoc.txt","w",encoding="utf-8").write(txt)
print("pages",len(d),"chars",len(txt))
