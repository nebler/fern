const fs = require('node:fs');

const directory = '/home/user/.local/share/opencode';

// Open the inode once so an atomic host-side refresh cannot race stat/read.
function readPrivate(name) {
  let fd;
  try {
    fd = fs.openSync(`${directory}/${name}`, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
    const stat = fs.fstatSync(fd);
    if (!stat.isFile() || stat.uid !== process.getuid() || (stat.mode & 0o777) !== 0o600 || stat.size > 4096) return null;
    const value = fs.readFileSync(fd, 'utf8');
    return value.endsWith('\n') ? value.slice(0, -1) : value;
  } catch {
    return null;
  } finally {
    if (fd !== undefined) fs.closeSync(fd);
  }
}

function token() {
  const value = readPrivate('fern-github-token');
  return value && /^[A-Za-z0-9_]+$/.test(value) ? value : null;
}

function repository() {
  const value = readPrivate('fern-github-repository');
  return value && /^[A-Za-z0-9][A-Za-z0-9-]*\/[A-Za-z0-9_.-]+$/.test(value)
    && !['.', '..'].includes(value.split('/')[1]) ? value : null;
}

module.exports = { directory, token, repository };
