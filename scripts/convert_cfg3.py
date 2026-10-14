import re

path = "src/downstream/configuration.go"
src = open(path).read().split("\n")

ovr2 = re.compile(
    r'^(\s*)Logger\.Print\("Overriding (\w+) with environment variable: " \+ prefix \+ "(\w+) with value: " \+ (\w+)\)$'
)
fatal2 = re.compile(
    r'^(\s*)Logger\.Fatalf\("Error in env var %s(\w+)\. Illegal value: %s\.(.*?)", prefix, (\w+)\)$'
)

envmap = {}
for line in src:
    m = ovr2.match(line)
    if m:
        envmap[m.group(3)] = m.group(2)

out = []
nov = nfa = 0
for line in src:
    m = ovr2.match(line)
    if m:
        indent, setting, env, var = m.groups()
        out.append(
            f'{indent}Logger.Info(configStr(logging.EventConfigOverride, "{setting}", {var}).With(\n'
            f'{indent}\tlogging.Str("airgap.config.source", prefix+"{env}"),\n'
            f'{indent}), "Overriding {setting} with environment variable: "+prefix+"{env} with value: "+{var})'
        )
        nov += 1
        continue
    m = fatal2.match(line)
    if m:
        indent, env, tail, var = m.groups()
        setting = envmap.get(env, env)
        out.append(
            f'{indent}Logger.Fatalf(configInvalid("{setting}", {var}).With(\n'
            f'{indent}\tlogging.Str("airgap.config.source", prefix+"{env}"),\n'
            f'{indent}), "Error in env var %s{env}. Illegal value: %s.{tail}", prefix, {var})'
        )
        nfa += 1
        continue
    out.append(line)

open(path, "w").write("\n".join(out))
print("overrides", nov, "fatals", nfa, envmap)
