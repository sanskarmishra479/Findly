export namespace main {
	
	export class Info {
	    version: string;
	    hotkeyWarning: string;
	    dataDir: string;
	    indexError: string;
	    modelNote: string;
	
	    static createFrom(source: any = {}) {
	        return new Info(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.hotkeyWarning = source["hotkeyWarning"];
	        this.dataDir = source["dataDir"];
	        this.indexError = source["indexError"];
	        this.modelNote = source["modelNote"];
	    }
	}

}

