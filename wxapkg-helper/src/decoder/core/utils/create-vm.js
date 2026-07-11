/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Adapted from wedecode v0.10.6 (commit 9bf626224511a42f77e4b682756472e06953854e).
 */

import vm from 'node:vm';
import { JSDOM } from "jsdom";
import { deepmerge } from "@biggerstar/deepmerge";
import process from "node:process";
import { createWxFakeDom } from "./wx-dom.js";
export function createVM(vmOptions = {}) {
    const domBaseHtml = `<!DOCTYPE html><html lang="en"><head><title>''</title></head><body></body></html>`;
    const dom = new JSDOM(domBaseHtml);
    const vm_window = dom.window;
    const vm_navigator = dom.window.navigator;
    const vm_document = dom.window.document;
    const __wxAppCode__ = {};
    const fakeGlobal = {
        __wxAppCode__,
        publishDomainComponents: () => void 0,
    };
    Object.assign(vm_window, fakeGlobal);
    const defaultOptions = {
        sandbox: {
            ...createWxFakeDom(),
            setInterval: () => null,
            setTimeout: () => null,
            console: {
                ...console, // 在 vm 执行的时候，对于小程序源码中的 info, log, warn 打印直接忽略
                log: () => void 0,
                warn: () => void 0,
                info: () => void 0,
            },
            window: vm_window,
            location: dom.window.location,
            navigator: vm_navigator,
            document: vm_document,
            define: () => void 0,
            require: () => void 0,
            requirePlugin: () => void 0,
            global: {
                __wcc_version__: 'v0.5vv_20211229_syb_scopedata',
            },
            System: {
                register: () => void 0,
            },
            __vd_version_info__: {},
            __wxAppCode__,
            __wxCodeSpace__: {
                setRuntimeGlobals: () => void 0,
                addComponentStaticConfig: () => void 0,
                setStyleScope: () => void 0,
                enableCodeChunk: () => void 0,
                initializeCodeChunk: () => void 0,
                addTemplateDependencies: () => void 0,
                batchAddCompiledScripts: () => void 0,
                batchAddCompiledTemplate: () => void 0,
            },
        }
    };
    const timeout = Number(process.env.WXAPKG_VM_TIMEOUT_MS);
    if (Number.isFinite(timeout) && timeout > 0) {
        defaultOptions.timeout = Math.floor(timeout);
    }
    return new DecoderVM(deepmerge(defaultOptions, vmOptions));
}
export function runVmCode(vm, code) {
    return vm.run(code);
}

class DecoderVM {
    constructor(options = {}) {
        this.timeout = normalizeTimeout(options.timeout);
        this.sandbox = options.sandbox || {};
        this.context = vm.createContext(this.sandbox, {
            name: 'wxapkg-helper-decoder',
            codeGeneration: {
                strings: true,
                wasm: false
            }
        });
    }

    run(code) {
        return vm.runInContext(code, this.context, {
            displayErrors: true,
            timeout: this.timeout
        });
    }
}

function normalizeTimeout(value) {
    const timeout = Number(value);
    return Number.isFinite(timeout) && timeout > 0 ? Math.floor(timeout) : 15000;
}
