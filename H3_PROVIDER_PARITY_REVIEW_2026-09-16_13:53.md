<｜DSML｜tool_calls>
<｜DSML｜invoke name="shell_command">
<｜DSML｜parameter name="cmd" string="true">cd /home/klets/ur/h3-provider && rg -n "TODO|FIXME|XXX|stub|not implemented|panic\(\"|unimplemented|IMPLEMENT" --glob '*.go' | head -200</｜DSML｜parameter>
<｜DSML｜parameter name="justification" string="true"></｜DSML｜parameter>
</｜DSML｜invoke>
<｜DSML｜invoke name="shell_command">
<｜DSML｜parameter name="cmd" string="true">cd /home/klets/ur/h3-provider && rg -n "func .*stub|STUB|stub" --glob '*.go' | head -200</｜DSML｜parameter>
<｜DSML｜parameter name="justification" string="true"></｜DSML｜parameter>
</｜DSML｜invoke>
<｜DSML｜invoke name="shell_command">
<｜DSML｜parameter name="cmd" string="true">cd /home/klets/ur/h3-provider && ls -la && find . -maxdepth 2 -type f -name '*.go' | sort | head -200</｜DSML｜parameter>
<｜DSML｜parameter name="justification" string="true"></｜DSML｜parameter>
</｜DSML｜invoke>
</｜DSML｜tool_calls>