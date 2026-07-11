/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Adapted from wedecode v0.10.6 (commit 9bf626224511a42f77e4b682756472e06953854e).
 */

import fs from "node:fs";
import path from "node:path";
import { pluginDirRename } from "../constant/index.js";

const DELETED = Symbol('deleted');

let allowedReadRoots = null;
let allowedWriteRoot = null;
let captureWrites = false;
let fileOverlay = new Map();

export function configureFileAccess(options = {}) {
    allowedReadRoots = normalizeRoots(options.readRoots);
    allowedWriteRoot = options.writeRoot ? path.resolve(options.writeRoot) : null;
    captureWrites = Boolean(options.captureWrites);
    fileOverlay = new Map();
}

/**
 * 读取文件，没有文件或者文件为空返回空字符串
 * */
export function readLocalFile(filePath, encoding = 'utf-8') {
    const data = readLocalBuffer(filePath);
    return data === null ? '' : data.toString(encoding);
}

/**
 * 读取文件，没有文件或者文件为空返回 null
 * */
export function readLocalJsonFile(filePath, encoding = 'utf-8') {
    try {
        return JSON.parse(readLocalFile(filePath, encoding));
    }
    catch (e) {
        if (e?.code === 'WXAPKG_CORE_FS_DENIED') {
            throw e;
        }
        return null;
    }
}

/**
 * 顺序读取列表中的文件， 直到读取的文件包含内容
 * */
export function readFileUntilContainContent(pathList, encoding = 'utf-8') {
    for (const filePath of pathList) {
        const data = readLocalBuffer(filePath);
        if (data?.length) {
            return {
                found: true,
                data: data.toString(encoding),
                path: filePath
            };
        }
    }
    return {
        found: false,
        data: '',
        path: ''
    };
}

/**
 * @param {string} filepath
 * @param {any} data
 * @param {Object} opt
 * @param {boolean} opt.force 是否强制覆盖, 默认为 false
 * @param {boolean} opt.emptyInstead 如果文原始件为空则允许覆盖
 * */
export function saveLocalFile(filepath, data, opt = {}) {
    filepath = filepath.replace(pluginDirRename[0], pluginDirRename[1]); // 重定向插件路径
    const target = assertWriteAllowed(filepath);
    const targetData = readLocalBuffer(target)?.toString('utf-8').trim() || '';
    const force = typeof opt.force === 'boolean' ? opt.force : opt.emptyInstead || !targetData.length;
    const isExistsFile = localFileExists(target);

    if (isExistsFile && !force)
        return false;

    const buffer = toBuffer(data);
    if (captureWrites) {
        fileOverlay.set(target, buffer);
    }
    else {
        fs.mkdirSync(path.dirname(target), { recursive: true });
        fs.writeFileSync(target, buffer);
    }
    return true;
}

export function deleteLocalFile(filePath, opt = {}) {
    try {
        const target = assertWriteAllowed(filePath);
        if (captureWrites) {
            fileOverlay.set(target, DELETED);
        }
        else {
            fs.rmSync(target, opt);
        }
    }
    catch (e) {
        if (!opt.catch)
            throw e;
    }
}

export function localFileExists(filePath) {
    const target = assertReadAllowed(filePath);
    if (fileOverlay.has(target)) {
        return fileOverlay.get(target) !== DELETED;
    }
    try {
        return fs.statSync(target).isFile();
    }
    catch {
        return false;
    }
}

export function listLocalFiles(rootPath, options = {}) {
    const root = assertReadAllowed(rootPath);
    const recursive = options.recursive !== false;
    const files = new Set();

    collectDiskFiles(root, recursive, files);

    for (const [target, value] of fileOverlay) {
        if (!isInside(root, target) || (!recursive && path.dirname(target) !== root)) {
            continue;
        }
        if (value === DELETED) {
            files.delete(target);
        }
        else {
            files.add(target);
        }
    }

    return [...files].sort();
}

export function getFileManifest() {
    if (!captureWrites || !allowedWriteRoot) {
        return [];
    }

    return [...fileOverlay.entries()].map(([target, value]) => {
        const relativePath = toManifestPath(allowedWriteRoot, target);
        if (value === DELETED) {
            return { op: 'delete', path: relativePath };
        }
        return {
            op: 'write',
            path: relativePath,
            data: value.toString('base64'),
            size: value.length
        };
    });
}

function readLocalBuffer(filePath) {
    const target = assertReadAllowed(filePath);
    if (fileOverlay.has(target)) {
        const value = fileOverlay.get(target);
        return value === DELETED ? null : Buffer.from(value);
    }
    try {
        return fs.readFileSync(target);
    }
    catch (error) {
        if (error?.code === 'ENOENT' || error?.code === 'EISDIR') {
            return null;
        }
        throw error;
    }
}

function collectDiskFiles(root, recursive, files) {
    let entries;
    try {
        entries = fs.readdirSync(root, { withFileTypes: true });
    }
    catch (error) {
        if (error?.code === 'ENOENT' || error?.code === 'ENOTDIR') {
            return;
        }
        throw error;
    }

    for (const entry of entries) {
        const target = path.join(root, entry.name);
        if (entry.isSymbolicLink()) {
            continue;
        }
        if (entry.isFile()) {
            files.add(target);
        }
        else if (recursive && entry.isDirectory()) {
            collectDiskFiles(target, true, files);
        }
    }
}

function assertReadAllowed(filePath) {
    const target = path.resolve(filePath);
    if (allowedReadRoots && !allowedReadRoots.some((root) => isInside(root, target))) {
        throw accessError('读取', target);
    }
    return target;
}

function assertWriteAllowed(filePath) {
    const target = path.resolve(filePath);
    if (allowedWriteRoot && (!isInside(allowedWriteRoot, target) || target === allowedWriteRoot)) {
        throw accessError('写入', target);
    }
    return target;
}

function normalizeRoots(roots) {
    if (!Array.isArray(roots)) {
        return null;
    }
    return [...new Set(roots.filter(Boolean).map((root) => path.resolve(root)))];
}

function isInside(root, target) {
    const relative = path.relative(root, target);
    return relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative));
}

function toManifestPath(root, target) {
    const relative = path.relative(root, target);
    if (!relative || relative === '..' || relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative)) {
        throw accessError('写入', target);
    }
    return relative.split(path.sep).join('/');
}

function toBuffer(data) {
    if (typeof data === 'string') {
        return Buffer.from(data);
    }
    if (Buffer.isBuffer(data) || ArrayBuffer.isView(data)) {
        return Buffer.from(data.buffer, data.byteOffset, data.byteLength);
    }
    throw new TypeError('反编译核心只能写入字符串或二进制数据。');
}

function accessError(operation, target) {
    const error = new Error(`反编译核心拒绝${operation}允许目录之外的路径：${target}`);
    error.code = 'WXAPKG_CORE_FS_DENIED';
    return error;
}
