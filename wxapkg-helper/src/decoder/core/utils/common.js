/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Adapted from wedecode v0.10.6 (commit 9bf626224511a42f77e4b682756472e06953854e).
 */

import path from "node:path";
import process from "node:process";
import JS from 'js-beautify';
export function getPathResolveInfo(outputDir) {
    let _packRootPath = outputDir;
    const resolve = (_new_resolve_path = './', ...args) => {
        return path.resolve(outputDir, _packRootPath, _new_resolve_path, ...args);
    };
    const outputResolve = (_new_resolve_path = './', ...args) => {
        return path.resolve(outputDir, _new_resolve_path, ...args);
    };
    return {
        /** 相对当前包( 子包， 主包， 插件包都算当前路径 )作为根路径路径进行解析 */
        resolve,
        /** 相对当前主包路径进行解析 */
        outputResolve,
        outputPath: outputDir,
        join(_path) {
            return path.join(_packRootPath, _path);
        },
        setPackRootPath(rootPath) {
            _packRootPath = rootPath;
        },
        /**
         * 当前的包根路径， 主包为 ./ , 分包为相对主包根的相对路径
         * */
        get packRootPath() {
            return _packRootPath === outputDir ? './' : _packRootPath;
        },
        get appJsonPath() {
            return resolve('app.json');
        },
        get appConfigJsonPath() {
            return resolve('app-config.json');
        },
        get projectPrivateConfigJsonPath() {
            return resolve('project.private.config.json');
        },
        get appWxssPath() {
            return resolve('app-wxss.js');
        },
        get workersPath() {
            return resolve('workers.js');
        },
        get pageFramePath() {
            return resolve('page-frame.js');
        },
        get pageFrameHtmlPath() {
            return resolve('page-frame.html');
        },
        get appJsPath() {
            return resolve('app.js');
        },
        get appServicePath() {
            return resolve('app-service.js');
        },
        get appServiceAppPath() {
            return resolve('appservice.app.js');
        },
        get gameJsonPath() {
            return resolve('game.json');
        },
        get gameJsPath() {
            return resolve('game.js');
        },
        get subContextJsPath() {
            return resolve('subContext.js');
        },
    };
}
export function jsBeautify(code) {
    return JS.js_beautify(code, { indent_size: 2 });
}
export function toPosixPath(filePath) {
    return String(filePath).replaceAll('\\', '/');
}
/** 深度遍历 */
export function traverseDOMTree(parentElement, astVNode, callback) {
    if (!astVNode)
        return;
    const newElement = callback(parentElement, astVNode);
    if (!newElement)
        return;
    const VNodeChildren = Array.from(astVNode.children).filter(Boolean);
    if (!VNodeChildren.length)
        return;
    for (let i = 0; i < VNodeChildren.length; i++) {
        traverseDOMTree(newElement, VNodeChildren[i], callback);
    }
}
export function clearScreen() {
    process.stdout.write(process.platform === 'win32' ? '\x1Bc' : '\x1B[2J\x1B[3J\x1B[H');
}
export function limitPush(arr, data, limit = 10) {
    if (arr.length - 1 > limit)
        arr.shift();
    arr.push(data);
}
export function printLog(log, opt = {}) {
    if (typeof opt.interceptor === 'function' && opt.interceptor(log) === false) {
        return;
    }
    console.log(log);
}
/**
 * 从数组中移除某个值
 * */
export function removeElement(arr, elementToRemove) {
    const index = arr.indexOf(elementToRemove);
    if (index > -1) {
        arr.splice(index, 1);
    }
}
/**
 * 获取公共的最长目录
 * */
export function commonDir(pathA, pathB) {
    if (pathA[0] === ".")
        pathA = pathA.slice(1);
    if (pathB[0] === ".")
        pathB = pathB.slice(1);
    pathA = pathA.replace(/\\/g, '/');
    pathB = pathB.replace(/\\/g, '/');
    let a = Math.min(pathA.length, pathB.length);
    for (let i = 1, m = Math.min(pathA.length, pathB.length); i <= m; i++)
        if (!pathA.startsWith(pathB.slice(0, i))) {
            a = i - 1;
            break;
        }
    let pub = pathB.slice(0, a);
    let len = pub.lastIndexOf("/") + 1;
    return pathA.slice(0, len);
}
/** 获取共同的最短根路径 */
export function findCommonRoot(paths) {
    const splitPaths = paths.map(path => path.split('/').filter(Boolean));
    const commonRoot = [];
    for (let i = 0; i < splitPaths[0].length; i++) {
        const partsMatch = splitPaths.every(path => path[i] === splitPaths[0][i]);
        if (partsMatch) {
            commonRoot.push(splitPaths[0][i]);
        }
        else {
            break;
        }
    }
    return commonRoot.join('/');
}
export function replaceExt(name, ext = "") {
    const hasSuffix = name.lastIndexOf(".") > 2; // x.x
    return hasSuffix ? name.slice(0, name.lastIndexOf(".")) + ext : `${name}${ext}`;
}
export function sleep(time) {
    return new Promise(resolve1 => setTimeout(resolve1, time));
}
/**
 * 数组去重， 回调函数返回布尔值，代表本次的成员是否添加到数组中, 返回 true 允许加入， 反之
 * 如果未传入回调函数， 将默认去重
 * */
export function arrayDeduplication(arr, cb) {
    return arr.reduce((pre, cur) => {
        const res = cb ? cb(pre, cur) : void 0;
        const isRes = typeof res === 'boolean';
        isRes ? res && pre.push(cur) : (!pre.includes(cur) && pre.push(cur));
        return pre;
    }, []);
}
export function removeVM2ExceptionLine(code) {
    const reg = /\s*[a-z]\x20?=\x20?VM2_INTERNAL_STATE_DO_NOT_USE_OR_PROGRAM_WILL_FAIL\.handleException\([a-z]\);?/g;
    return code.replace(reg, '');
}
export function resetWxsRequirePath(p, resetString = '') {
    return p.replaceAll('p_./', resetString).replaceAll('m_./', resetString);
}
export function isPluginPath(path) {
    return path.startsWith('plugin-private://') || path.startsWith('plugin://');
}
export function resetPluginPath(_path, prefixDir) {
    return path.join(prefixDir || './', _path.replaceAll('plugin-private://', '').replaceAll('plugin://', ''));
}
/**
 * 获取某个函数的入参定义的名称
 * */
export function getParameterNames(fn) {
    if (typeof fn !== 'function')
        return [];
    const COMMENTS = /((\/\/.*$)|(\/\*[\s\S]*?\*\/))/mg;
    const code = fn.toString().replace(COMMENTS, '');
    const result = code.slice(code.indexOf('(') + 1, code.indexOf(')'))
        .match(/([^\s,]+)/g);
    return result === null
        ? []
        : result;
}
/**
 * 判断是否是wx 的 appid
 * */
export function isWxAppid(str) {
    const reg = /^wx[0-9a-f]{16}$/i;
    str = str.trim();
    return str.length === 18 && reg.test(str);
}
